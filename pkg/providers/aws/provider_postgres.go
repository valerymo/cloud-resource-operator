package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"

	croType "github.com/integr8ly/cloud-resource-operator/pkg/apis/integreatly/v1alpha1/types"

	"github.com/aws/aws-sdk-go/aws/awserr"

	"k8s.io/apimachinery/pkg/types"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds/rdsiface"
	"github.com/integr8ly/cloud-resource-operator/pkg/resources"

	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/integr8ly/cloud-resource-operator/pkg/apis/integreatly/v1alpha1"
	"github.com/integr8ly/cloud-resource-operator/pkg/providers"
	"github.com/sirupsen/logrus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	errorUtil "github.com/pkg/errors"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	postgresProviderName                 = "aws-rds"
	DefaultAwsIdentifierLength           = 40
	defaultAwsMultiAZ                    = true
	defaultAwsPostgresDeletionProtection = true
	defaultAwsPostgresPort               = 5432
	defaultAwsPostgresUser               = "postgres"
	defaultAwsAllocatedStorage           = 20
	defaultAwsMaxAllocatedStorage        = 100
	defaultAwsPostgresDatabase           = "postgres"
	defaultAwsBackupRetentionPeriod      = 31
	defaultAwsDBInstanceClass            = "db.t2.small"
	defaultAwsEngine                     = "postgres"
	defaultAwsEngineVersion              = "10.6"
	defaultAwsPubliclyAccessible         = false
	defaultAwsSkipFinalSnapshot          = false
	defaultAwsDeleteAutomatedBackups     = true
	defaultCredSecSuffix                 = "-aws-rds-credentials"
	defaultPostgresUserKey               = "user"
	defaultPostgresPasswordKey           = "password"
	defaultStorageEncrypted              = true
)

var (
	defaultSupportedEngineVersions = []string{"10.6", "9.6", "9.5"}
)

var _ providers.PostgresProvider = (*PostgresProvider)(nil)

type PostgresProvider struct {
	Client            client.Client
	Logger            *logrus.Entry
	CredentialManager CredentialManager
	ConfigManager     ConfigManager
	TCPPinger         ConnectionTester
}

func NewAWSPostgresProvider(client client.Client, logger *logrus.Entry) *PostgresProvider {
	return &PostgresProvider{
		Client:            client,
		Logger:            logger.WithFields(logrus.Fields{"provider": postgresProviderName}),
		CredentialManager: NewCredentialMinterCredentialManager(client),
		ConfigManager:     NewDefaultConfigMapConfigManager(client),
		TCPPinger:         NewConnectionTestManager(),
	}
}

func (p *PostgresProvider) GetName() string {
	return postgresProviderName
}

func (p *PostgresProvider) SupportsStrategy(d string) bool {
	return d == providers.AWSDeploymentStrategy
}

func (p *PostgresProvider) GetReconcileTime(pg *v1alpha1.Postgres) time.Duration {
	if pg.Status.Phase != croType.PhaseComplete {
		return time.Second * 60
	}
	return resources.GetForcedReconcileTimeOrDefault(defaultReconcileTime)
}

// CreatePostgres creates an RDS Instance from strategy config
func (p *PostgresProvider) CreatePostgres(ctx context.Context, pg *v1alpha1.Postgres) (*providers.PostgresInstance, croType.StatusMessage, error) {
	// handle provider-specific finalizer
	if err := resources.CreateFinalizer(ctx, p.Client, pg, DefaultFinalizer); err != nil {
		return nil, "failed to set finalizer", err
	}

	// info about the RDS instance to be created
	rdsCfg, _, stratCfg, err := p.getRDSConfig(ctx, pg)
	if err != nil {
		msg := "failed to retrieve aws rds cluster config for instance"
		return nil, croType.StatusMessage(msg), errorUtil.Wrapf(err, msg)
	}

	// create the credentials to be used by the aws resource providers, not to be used by end-user
	providerCreds, err := p.CredentialManager.ReconcileProviderCredentials(ctx, pg.Namespace)
	if err != nil {
		msg := "failed to reconcile rds credentials"
		return nil, croType.StatusMessage(msg), errorUtil.Wrap(err, msg)
	}

	// create credentials secret
	sec := buildDefaultRDSSecret(pg)
	or, err := controllerutil.CreateOrUpdate(ctx, p.Client, sec, func() error {
		return nil
	})
	if err != nil {
		errMsg := fmt.Sprintf("failed to create or update secret %s, action was %s", sec.Name, or)
		return nil, croType.StatusMessage(errMsg), errorUtil.Wrapf(err, errMsg)
	}

	// setup aws RDS instance sdk session
	sess, err := CreateSessionFromStrategy(ctx, p.Client, providerCreds.AccessKeyID, providerCreds.SecretAccessKey, stratCfg)
	if err != nil {
		errMsg := "failed to create aws session to create rds db instance"
		return nil, croType.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}
	// create the aws RDS instance
	return p.createRDSInstance(ctx, pg, rds.New(sess), ec2.New(sess), rdsCfg)
}

func (p *PostgresProvider) createRDSInstance(ctx context.Context, cr *v1alpha1.Postgres, rdsSvc rdsiface.RDSAPI, ec2Svc ec2iface.EC2API, rdsCfg *rds.CreateDBInstanceInput) (*providers.PostgresInstance, croType.StatusMessage, error) {
	// the aws access key can sometimes still not be registered in aws on first try, so loop
	pi, err := getRDSInstances(rdsSvc)
	if err != nil {
		// return nil error so this function can be requeued
		msg := "error getting replication groups"
		return nil, croType.StatusMessage(msg), err
	}

	// setup vpc
	if err := p.configureRDSVpc(ctx, rdsSvc, ec2Svc); err != nil {
		errMsg := "error setting up resource vpc"
		return nil, croType.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}

	// setup security group
	if err := configureSecurityGroup(ctx, p.Client, ec2Svc); err != nil {
		errMsg := "error setting up security group"
		return nil, croType.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}

	// getting postgres user password from created secret
	credSec := &v1.Secret{}
	if err := p.Client.Get(ctx, types.NamespacedName{Name: cr.Name + defaultCredSecSuffix, Namespace: cr.Namespace}, credSec); err != nil {
		msg := "failed to retrieve rds credential secret"
		return nil, croType.StatusMessage(msg), errorUtil.Wrap(err, msg)
	}

	postgresPass := string(credSec.Data[defaultPostgresPasswordKey])
	if postgresPass == "" {
		msg := "unable to retrieve rds password"
		return nil, croType.StatusMessage(msg), errorUtil.Wrap(err, msg)
	}

	// verify and build rds create config
	if err := p.buildRDSCreateStrategy(ctx, cr, ec2Svc, rdsCfg, postgresPass); err != nil {
		msg := "failed to build and verify aws rds instance configuration"
		return nil, croType.StatusMessage(msg), errorUtil.Wrap(err, msg)
	}

	// check if the cluster has already been created
	var foundInstance *rds.DBInstance
	for _, i := range pi {
		if *i.DBInstanceIdentifier == *rdsCfg.DBInstanceIdentifier {
			foundInstance = i
			break
		}
	}

	// create rds instance if it doesn't exist
	if foundInstance == nil {
		logrus.Info("creating rds instance")
		if _, err = rdsSvc.CreateDBInstance(rdsCfg); err != nil {
			return nil, croType.StatusMessage(fmt.Sprintf("error creating rds instance %s", err)), err
		}
		return nil, "started rds provision", nil
	}

	// expose pending maintenance metric
	p.setPostgresServiceMaintenanceMetric(ctx, cr, rdsSvc, foundInstance)

	// set status metric
	p.exposePostgresMetrics(ctx, cr, foundInstance)

	// check rds instance phase
	if *foundInstance.DBInstanceStatus != "available" {
		return nil, croType.StatusMessage(fmt.Sprintf("createRDSInstance() in progress, current aws rds resource status is %s", *foundInstance.DBInstanceStatus)), nil
	}

	// check if found instance and user strategy differs, and modify instance
	logrus.Info("found existing rds instance")
	mi := buildRDSUpdateStrategy(rdsCfg, foundInstance)
	if mi != nil {
		if _, err = rdsSvc.ModifyDBInstance(mi); err != nil {
			return nil, "failed to modify instance", err
		}
		return nil, croType.StatusMessage(fmt.Sprintf("changes detected, modifyDBInstance() in progress, current aws rds resource status is %s", *foundInstance.DBInstanceStatus)), nil
	}
	// Add Tags to Aws Postgres resources
	msg, err := p.TagRDSPostgres(ctx, cr, rdsSvc, foundInstance)
	if err != nil {
		errMsg := fmt.Sprintf("failed to add tags to rds: %s", msg)
		return nil, croType.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}

	pdd := &providers.PostgresDeploymentDetails{
		Username: *foundInstance.MasterUsername,
		Password: postgresPass,
		Host:     *foundInstance.Endpoint.Address,
		Database: *foundInstance.DBName,
		Port:     int(*foundInstance.Endpoint.Port),
	}

	// create connection metric
	p.createRDSConnectionMetric(ctx, cr, foundInstance, pdd)

	// return secret information
	return &providers.PostgresInstance{DeploymentDetails: pdd}, croType.StatusMessage(fmt.Sprintf("%s, aws rds status is %s", msg, *foundInstance.DBInstanceStatus)), nil
}

// TagRDSPostgres Tags RDS resources
func (p *PostgresProvider) TagRDSPostgres(ctx context.Context, cr *v1alpha1.Postgres, rdsSvc rdsiface.RDSAPI, foundInstance *rds.DBInstance) (croType.StatusMessage, error) {
	logrus.Infof("adding tags to rds instance %s", *foundInstance.DBInstanceIdentifier)
	// get the environment from the CR
	// set the tag values that will always be added
	defaultOrganizationTag := resources.GetOrganizationTag()

	//get Cluster Id
	clusterID, _ := resources.GetClusterID(ctx, p.Client)
	// Set the Tag values

	rdsTag := []*rds.Tag{
		{
			Key:   aws.String(defaultOrganizationTag + "clusterID"),
			Value: aws.String(clusterID),
		},
		{
			Key:   aws.String(defaultOrganizationTag + "resource-type"),
			Value: aws.String(cr.Spec.Type),
		},
		{
			Key:   aws.String(defaultOrganizationTag + "resource-name"),
			Value: aws.String(cr.Name),
		},
	}
	if cr.ObjectMeta.Labels["productName"] != "" {
		productTag := &rds.Tag{
			Key:   aws.String(defaultOrganizationTag + "product-name"),
			Value: aws.String(cr.ObjectMeta.Labels["productName"]),
		}
		rdsTag = append(rdsTag, productTag)
	}

	// adding tags to rds postgres instance
	_, err := rdsSvc.AddTagsToResource(&rds.AddTagsToResourceInput{
		ResourceName: aws.String(*foundInstance.DBInstanceArn),
		Tags:         rdsTag,
	})
	if err != nil {
		msg := "Failed to add Tags to RDS instance"
		return croType.StatusMessage(msg), errorUtil.Wrapf(err, msg)

	}

	// Get a list of Snapshot objects for the DB instance
	rdsSnapshotAttributeInput := &rds.DescribeDBSnapshotsInput{
		DBInstanceIdentifier: aws.String(*foundInstance.DBInstanceIdentifier),
	}
	rdsSnapshotList, err := rdsSvc.DescribeDBSnapshots(rdsSnapshotAttributeInput)
	if err != nil {
		msg := "Can't get Snapshot info"
		return croType.StatusMessage(msg), errorUtil.Wrapf(err, msg)
	}
	// Adding tags to each DB Snapshots from list on AWS
	for _, snapshotList := range rdsSnapshotList.DBSnapshots {
		inputRdsSnapshot := &rds.AddTagsToResourceInput{
			ResourceName: aws.String(*snapshotList.DBSnapshotArn),
			Tags:         rdsTag,
		}
		// Adding Tags to RDS Snapshot
		_, err = rdsSvc.AddTagsToResource(inputRdsSnapshot)
		if err != nil {
			msg := "Failed to add Tags to RDS Snapshot"
			return croType.StatusMessage(msg), errorUtil.Wrapf(err, msg)
		}
	}

	logrus.Infof("tags were added successfully to the rds instance %s", *foundInstance.DBInstanceIdentifier)
	return "successfully created and tagged", nil
}

func (p *PostgresProvider) DeletePostgres(ctx context.Context, r *v1alpha1.Postgres) (croType.StatusMessage, error) {
	// resolve postgres information for postgres created by provider
	rdsCreateConfig, rdsDeleteConfig, stratCfg, err := p.getRDSConfig(ctx, r)
	if err != nil {
		return "failed to retrieve aws rds config", err
	}

	// get provider aws creds so the postgres instance can be deleted
	providerCreds, err := p.CredentialManager.ReconcileProviderCredentials(ctx, r.Namespace)
	if err != nil {
		msg := "failed to reconcile aws provider credentials"
		return croType.StatusMessage(msg), errorUtil.Wrap(err, msg)
	}

	// setup aws postgres instance sdk session
	sess, err := CreateSessionFromStrategy(ctx, p.Client, providerCreds.AccessKeyID, providerCreds.SecretAccessKey, stratCfg)
	if err != nil {
		errMsg := "failed to create aws session to delete rds db instance"
		return croType.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}

	return p.deleteRDSInstance(ctx, r, rds.New(sess), rdsCreateConfig, rdsDeleteConfig)
}

func (p *PostgresProvider) deleteRDSInstance(ctx context.Context, pg *v1alpha1.Postgres, instanceSvc rdsiface.RDSAPI, rdsCreateConfig *rds.CreateDBInstanceInput, rdsDeleteConfig *rds.DeleteDBInstanceInput) (croType.StatusMessage, error) {
	// the aws access key can sometimes still not be registered in aws on first try, so loop
	pgs, err := getRDSInstances(instanceSvc)
	if err != nil {
		return "error getting aws rds instances", err
	}

	// check and verify delete config
	if err := p.buildRDSDeleteConfig(ctx, pg, rdsCreateConfig, rdsDeleteConfig); err != nil {
		msg := "failed to verify aws rds instance configuration"
		return croType.StatusMessage(msg), errorUtil.Wrap(err, msg)
	}

	// check if the instance has already been deleted
	var foundInstance *rds.DBInstance
	for _, i := range pgs {
		if *i.DBInstanceIdentifier == *rdsDeleteConfig.DBInstanceIdentifier {
			foundInstance = i
			break
		}
	}

	// check if instance does not exist, delete finalizer and credential secret
	if foundInstance == nil {
		// delete credential secret
		p.Logger.Info("deleting rds secret")
		sec := &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pg.Name + defaultCredSecSuffix,
				Namespace: pg.Namespace,
			},
		}
		err = p.Client.Delete(ctx, sec)
		if err != nil && !k8serr.IsNotFound(err) {
			msg := "failed to deleted rds secrets"
			return croType.StatusMessage(msg), errorUtil.Wrap(err, msg)
		}

		resources.RemoveFinalizer(&pg.ObjectMeta, DefaultFinalizer)
		if err := p.Client.Update(ctx, pg); err != nil {
			msg := "failed to update instance as part of finalizer reconcile"
			return croType.StatusMessage(msg), errorUtil.Wrapf(err, msg)
		}
		return croType.StatusEmpty, nil
	}

	// set status metric
	p.exposePostgresMetrics(ctx, pg, foundInstance)

	// return if rds instance is not available
	if *foundInstance.DBInstanceStatus != "available" {
		return croType.StatusMessage(fmt.Sprintf("delete detected, deleteDBInstance() in progress, current aws rds status is %s", *foundInstance.DBInstanceStatus)), nil
	}

	// delete rds instance if deletion protection is false
	if !*foundInstance.DeletionProtection {
		_, err = instanceSvc.DeleteDBInstance(rdsDeleteConfig)
		rdsErr, isAwsErr := err.(awserr.Error)
		if err != nil && (!isAwsErr || rdsErr.Code() != rds.ErrCodeDBInstanceNotFoundFault) {
			msg := fmt.Sprintf("failed to delete rds instance : %s", err)
			return croType.StatusMessage(msg), errorUtil.Wrapf(err, msg)
		}
		return "delete detected, deleteDBInstance() started", nil
	}

	// modify rds instance to turn off deletion protection
	_, err = instanceSvc.ModifyDBInstance(&rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: rdsDeleteConfig.DBInstanceIdentifier,
		DeletionProtection:   aws.Bool(false),
	})
	if err != nil {
		msg := "failed to remove deletion protection"
		return croType.StatusMessage(msg), errorUtil.Wrap(err, msg)
	}

	return croType.StatusMessage(fmt.Sprintf("deletion protection detected, modifyDBInstance() in progress, current aws rds status is %s", *foundInstance.DBInstanceStatus)), nil
}

// function to get rds instances, used to check/wait on AWS credentials
func getRDSInstances(cacheSvc rdsiface.RDSAPI) ([]*rds.DBInstance, error) {
	var pi []*rds.DBInstance
	err := wait.PollImmediate(time.Second*5, time.Minute*5, func() (done bool, err error) {
		listOutput, err := cacheSvc.DescribeDBInstances(&rds.DescribeDBInstancesInput{})
		if err != nil {
			return false, nil
		}
		pi = listOutput.DBInstances
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return pi, nil
}

func (p *PostgresProvider) getRDSConfig(ctx context.Context, r *v1alpha1.Postgres) (*rds.CreateDBInstanceInput, *rds.DeleteDBInstanceInput, *StrategyConfig, error) {
	stratCfg, err := p.ConfigManager.ReadStorageStrategy(ctx, providers.PostgresResourceType, r.Spec.Tier)
	if err != nil {
		return nil, nil, nil, errorUtil.Wrap(err, "failed to read aws strategy config")
	}

	defRegion, err := GetRegionFromStrategyOrDefault(ctx, p.Client, stratCfg)
	if err != nil {
		return nil, nil, nil, errorUtil.Wrap(err, "failed to get default region")
	}
	if stratCfg.Region == "" {
		p.Logger.Debugf("region not set in deployment strategy configuration, using default region %s", defRegion)
		stratCfg.Region = defRegion
	}

	rdsCreateConfig := &rds.CreateDBInstanceInput{}
	if err := json.Unmarshal(stratCfg.CreateStrategy, rdsCreateConfig); err != nil {
		return nil, nil, nil, errorUtil.Wrap(err, "failed to unmarshal aws rds cluster configuration")
	}

	rdsDeleteConfig := &rds.DeleteDBInstanceInput{}
	if err := json.Unmarshal(stratCfg.DeleteStrategy, rdsDeleteConfig); err != nil {
		return nil, nil, nil, errorUtil.Wrap(err, "failed to unmarshal aws rds cluster configuration")
	}
	return rdsCreateConfig, rdsDeleteConfig, stratCfg, nil
}

// verifies if there is a change between a found instance and the configuration from the instance strat
func buildRDSUpdateStrategy(rdsConfig *rds.CreateDBInstanceInput, foundConfig *rds.DBInstance) *rds.ModifyDBInstanceInput {
	updateFound := false

	mi := &rds.ModifyDBInstanceInput{}
	mi.DBInstanceIdentifier = foundConfig.DBInstanceIdentifier

	if *rdsConfig.DeletionProtection != *foundConfig.DeletionProtection {
		mi.DeletionProtection = rdsConfig.DeletionProtection
		updateFound = true
	}
	if *rdsConfig.Port != *foundConfig.Endpoint.Port {
		mi.DBPortNumber = rdsConfig.Port
		updateFound = true
	}
	if *rdsConfig.BackupRetentionPeriod != *foundConfig.BackupRetentionPeriod {
		mi.BackupRetentionPeriod = rdsConfig.BackupRetentionPeriod
		updateFound = true
	}
	if *rdsConfig.DBInstanceClass != *foundConfig.DBInstanceClass {
		mi.DBInstanceClass = rdsConfig.DBInstanceClass
		updateFound = true
	}
	if *rdsConfig.PubliclyAccessible != *foundConfig.PubliclyAccessible {
		mi.PubliclyAccessible = rdsConfig.PubliclyAccessible
		updateFound = true
	}
	if *rdsConfig.AllocatedStorage != *foundConfig.AllocatedStorage {
		mi.AllocatedStorage = rdsConfig.AllocatedStorage
		updateFound = true
	}
	if *rdsConfig.EngineVersion != *foundConfig.EngineVersion {
		mi.EngineVersion = rdsConfig.EngineVersion
		updateFound = true
	}
	if *rdsConfig.MultiAZ != *foundConfig.MultiAZ {
		mi.MultiAZ = rdsConfig.MultiAZ
		updateFound = true
	}
	if !updateFound {
		return nil
	}
	return mi
}

// verify postgres create config
func (p *PostgresProvider) buildRDSCreateStrategy(ctx context.Context, pg *v1alpha1.Postgres, ec2Svc ec2iface.EC2API, rdsCreateConfig *rds.CreateDBInstanceInput, postgresPassword string) error {
	if rdsCreateConfig.DeletionProtection == nil {
		rdsCreateConfig.DeletionProtection = aws.Bool(defaultAwsPostgresDeletionProtection)
	}
	if rdsCreateConfig.MasterUsername == nil {
		rdsCreateConfig.MasterUsername = aws.String(defaultAwsPostgresUser)
	}
	if rdsCreateConfig.MasterUserPassword == nil {
		rdsCreateConfig.MasterUserPassword = aws.String(postgresPassword)
	}
	if rdsCreateConfig.Port == nil {
		rdsCreateConfig.Port = aws.Int64(defaultAwsPostgresPort)
	}
	if rdsCreateConfig.DBName == nil {
		rdsCreateConfig.DBName = aws.String(defaultAwsPostgresDatabase)
	}
	if rdsCreateConfig.BackupRetentionPeriod == nil {
		rdsCreateConfig.BackupRetentionPeriod = aws.Int64(defaultAwsBackupRetentionPeriod)
	}
	if rdsCreateConfig.DBInstanceClass == nil {
		rdsCreateConfig.DBInstanceClass = aws.String(defaultAwsDBInstanceClass)
	}
	if rdsCreateConfig.PubliclyAccessible == nil {
		rdsCreateConfig.PubliclyAccessible = aws.Bool(defaultAwsPubliclyAccessible)
	}
	if rdsCreateConfig.AllocatedStorage == nil {
		rdsCreateConfig.AllocatedStorage = aws.Int64(defaultAwsAllocatedStorage)
	}
	if rdsCreateConfig.MaxAllocatedStorage == nil {
		rdsCreateConfig.MaxAllocatedStorage = aws.Int64(defaultAwsMaxAllocatedStorage)
	}
	if rdsCreateConfig.EngineVersion == nil {
		rdsCreateConfig.EngineVersion = aws.String(defaultAwsEngineVersion)
	}
	if rdsCreateConfig.StorageEncrypted == nil {
		rdsCreateConfig.StorageEncrypted = aws.Bool(defaultStorageEncrypted)
	}
	if rdsCreateConfig.EngineVersion != nil {
		if !resources.Contains(defaultSupportedEngineVersions, *rdsCreateConfig.EngineVersion) {
			rdsCreateConfig.EngineVersion = aws.String(defaultAwsEngineVersion)
		}
	}
	instanceName, err := BuildInfraNameFromObject(ctx, p.Client, pg.ObjectMeta, DefaultAwsIdentifierLength)
	if err != nil {
		return errorUtil.Wrapf(err, "failed to retrieve rds config")
	}
	if rdsCreateConfig.DBInstanceIdentifier == nil {
		rdsCreateConfig.DBInstanceIdentifier = aws.String(instanceName)
	}
	if rdsCreateConfig.MultiAZ == nil {
		rdsCreateConfig.MultiAZ = aws.Bool(defaultAwsMultiAZ)
	}
	if *rdsCreateConfig.MultiAZ {
		rdsCreateConfig.AvailabilityZone = nil
	}
	rdsCreateConfig.Engine = aws.String(defaultAwsEngine)
	subGroup, err := BuildInfraName(ctx, p.Client, defaultSubnetPostfix, DefaultAwsIdentifierLength)
	if err != nil {
		return errorUtil.Wrapf(err, "failed to build subnet group name")
	}
	if rdsCreateConfig.DBSubnetGroupName == nil {
		rdsCreateConfig.DBSubnetGroupName = aws.String(subGroup)
	}

	// build security group name
	secName, err := BuildInfraName(ctx, p.Client, defaultSecurityGroupPostfix, DefaultAwsIdentifierLength)
	if err != nil {
		return errorUtil.Wrap(err, "error building subnet group name")
	}
	// get security group
	foundSecGroup, err := getSecurityGroup(ec2Svc, secName)
	if err != nil {
		return errorUtil.Wrap(err, "")
	}

	if rdsCreateConfig.VpcSecurityGroupIds == nil {
		rdsCreateConfig.VpcSecurityGroupIds = []*string{
			aws.String(*foundSecGroup.GroupId),
		}
	}
	return nil
}

// verify postgres delete config
func (p *PostgresProvider) buildRDSDeleteConfig(ctx context.Context, pg *v1alpha1.Postgres, rdsCreateConfig *rds.CreateDBInstanceInput, rdsDeleteConfig *rds.DeleteDBInstanceInput) error {
	instanceIdentifier, err := BuildInfraNameFromObject(ctx, p.Client, pg.ObjectMeta, DefaultAwsIdentifierLength)
	if err != nil {
		return errorUtil.Wrapf(err, "failed to retrieve rds config")
	}
	if rdsDeleteConfig.DBInstanceIdentifier == nil {
		if rdsCreateConfig.DBInstanceIdentifier == nil {
			rdsCreateConfig.DBInstanceIdentifier = aws.String(instanceIdentifier)
		}
		rdsDeleteConfig.DBInstanceIdentifier = rdsCreateConfig.DBInstanceIdentifier
	}
	if rdsDeleteConfig.DeleteAutomatedBackups == nil {
		rdsDeleteConfig.DeleteAutomatedBackups = aws.Bool(defaultAwsDeleteAutomatedBackups)
	}
	if rdsDeleteConfig.SkipFinalSnapshot == nil {
		rdsDeleteConfig.SkipFinalSnapshot = aws.Bool(defaultAwsSkipFinalSnapshot)
	}
	snapshotIdentifier, err := buildTimestampedInfraNameFromObject(ctx, p.Client, pg.ObjectMeta, DefaultAwsIdentifierLength)
	if err != nil {
		return errorUtil.Wrap(err, "failed to retrieve timestamped rds config")
	}
	if rdsDeleteConfig.FinalDBSnapshotIdentifier == nil && !*rdsDeleteConfig.SkipFinalSnapshot {
		rdsDeleteConfig.FinalDBSnapshotIdentifier = aws.String(snapshotIdentifier)
	}
	return nil
}

func buildDefaultRDSSecret(ps *v1alpha1.Postgres) *v1.Secret {
	password, err := resources.GeneratePassword()
	if err != nil {
		return nil
	}
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ps.Name + defaultCredSecSuffix,
			Namespace: ps.Namespace,
		},
		StringData: map[string]string{
			defaultPostgresUserKey:     defaultAwsPostgresUser,
			defaultPostgresPasswordKey: password,
		},
		Type: v1.SecretTypeOpaque,
	}
}

// ensures a subnet group is in place to configure the resource to be in the same vpc as the cluster
func (p *PostgresProvider) configureRDSVpc(ctx context.Context, rdsSvc rdsiface.RDSAPI, ec2Svc ec2iface.EC2API) error {
	logrus.Info("ensuring vpc is as expected for resource")
	// get subnet group id
	sgID, err := BuildInfraName(ctx, p.Client, defaultSubnetPostfix, DefaultAwsIdentifierLength)
	if err != nil {
		return errorUtil.Wrap(err, "error building subnet group name")
	}

	// check if group exists
	groups, err := rdsSvc.DescribeDBSubnetGroups(&rds.DescribeDBSubnetGroupsInput{})
	if err != nil {
		return errorUtil.Wrap(err, "error describing subnet groups")
	}
	var foundSubnet *rds.DBSubnetGroup
	for _, sub := range groups.DBSubnetGroups {
		if *sub.DBSubnetGroupName == sgID {
			foundSubnet = sub
			break
		}
	}
	if foundSubnet != nil {
		logrus.Info(fmt.Sprintf("subnet group %s found", *foundSubnet.DBSubnetGroupName))
		return nil
	}

	// get cluster id
	clusterID, err := resources.GetClusterID(ctx, p.Client)
	if err != nil {
		return errorUtil.Wrap(err, "error getting cluster id")
	}

	// get cluster vpc subnets
	subIDs, err := GetPrivateSubnetIDS(ctx, p.Client, ec2Svc)
	if err != nil {
		return errorUtil.Wrap(err, "error getting vpc subnets")
	}

	// build subnet group input
	subnetGroupInput := &rds.CreateDBSubnetGroupInput{
		DBSubnetGroupDescription: aws.String(defaultSubnetGroupDesc),
		DBSubnetGroupName:        aws.String(sgID),
		SubnetIds:                subIDs,
		Tags: []*rds.Tag{
			{
				Key:   aws.String("cluster"),
				Value: aws.String(clusterID),
			},
		},
	}

	// create db subnet group
	logrus.Info("creating resource subnet group")
	if _, err := rdsSvc.CreateDBSubnetGroup(subnetGroupInput); err != nil {
		return errorUtil.Wrap(err, "unable to create db subnet group")
	}

	return nil
}

func buildPostgresInfoMetricLabels(cr *v1alpha1.Postgres, instance *rds.DBInstance, clusterID string) map[string]string {
	labels := buildPostgresGenericMetricLabels(cr, instance, clusterID)
	labels["status"] = *instance.DBInstanceStatus
	return labels
}

func buildPostgresGenericMetricLabels(cr *v1alpha1.Postgres, instance *rds.DBInstance, clusterID string) map[string]string {
	labels := map[string]string{}
	labels["clusterID"] = clusterID
	labels["resourceID"] = cr.Name
	labels["namespace"] = cr.Namespace
	labels["instanceID"] = *instance.DBInstanceIdentifier
	labels["productName"] = cr.Labels["productName"]
	labels["strategy"] = postgresProviderName
	return labels
}

func (p *PostgresProvider) exposePostgresMetrics(ctx context.Context, cr *v1alpha1.Postgres, instance *rds.DBInstance) {
	// get Cluster Id
	logrus.Info("setting postgres information metric")
	clusterID, err := resources.GetClusterID(ctx, p.Client)
	if err != nil {
		logrus.Error(fmt.Sprintf("failed to get cluster id while exposing information metric for %s", *instance.DBInstanceIdentifier))
		return
	}

	// build metric labels
	infoLabels := buildPostgresInfoMetricLabels(cr, instance, clusterID)
	// build available mertic labels
	genericLabels := buildPostgresGenericMetricLabels(cr, instance, clusterID)

	// set status gauge
	resources.SetMetricCurrentTime(resources.DefaultPostgresInfoMetricName, infoLabels)

	// set available metric
	if *instance.DBInstanceStatus != "available" {
		resources.SetMetric(resources.DefaultPostgresAvailMetricName, genericLabels, 0)
		return
	}
	resources.SetMetric(resources.DefaultPostgresAvailMetricName, genericLabels, 1)
}

func (p *PostgresProvider) setPostgresServiceMaintenanceMetric(ctx context.Context, cr *v1alpha1.Postgres, rdsSession rdsiface.RDSAPI, instance *rds.DBInstance) {
	logrus.Info("checking for pending postgres service updates")
	clusterID, err := resources.GetClusterID(ctx, p.Client)
	if err != nil {
		logrus.Error(fmt.Sprintf("failed to get cluster id while exposing information metric for %s", *instance.DBInstanceIdentifier))
		return
	}

	// Retrieve service maintenance updates, create and export Prometheus metrics
	output, err := rdsSession.DescribePendingMaintenanceActions(&rds.DescribePendingMaintenanceActionsInput{})
	if err != nil {
		logrus.Error(fmt.Sprintf("failed to get maintenance information while exposing maintenance metric for %s", *instance.DBInstanceIdentifier))
		return
	}

	logrus.Info(fmt.Sprintf("rds serviceupdates: %d available", len(output.PendingMaintenanceActions)))
	for _, su := range output.PendingMaintenanceActions {
		metricLabels := map[string]string{}

		metricLabels["clusterID"] = clusterID
		metricLabels["ResourceIdentifier"] = *su.ResourceIdentifier

		for _, pma := range su.PendingMaintenanceActionDetails {

			metricLabels["AutoAppliedAfterDate"] = strconv.FormatInt((*pma.AutoAppliedAfterDate).Unix(), 10)
			metricLabels["CurrentApplyDate"] = strconv.FormatInt((*pma.CurrentApplyDate).Unix(), 10)
			metricLabels["Description"] = *pma.Description

			metricEpochTimestamp := (*pma.AutoAppliedAfterDate).Unix()

			resources.SetMetric(resources.DefaultPostgresMaintenanceMetricName, metricLabels, float64(metricEpochTimestamp))
		}
	}

}

// tests to see if a simple tcp connection can be made to rds and creates a metric based on this
func (p *PostgresProvider) createRDSConnectionMetric(ctx context.Context, cr *v1alpha1.Postgres, instance *rds.DBInstance, postgresInstance *providers.PostgresDeploymentDetails) {
	// return cluster id needed for metric labels
	logrus.Info(fmt.Sprintf("testing and exposing postgres connection metric for: %s", *instance.DBInstanceIdentifier))
	clusterID, err := resources.GetClusterID(ctx, p.Client)
	if err != nil {
		logrus.Error(fmt.Sprintf("failed to get cluster id while exposing connection metric for %s", *instance.DBInstanceIdentifier))

	}

	// build generic labels to be added to metric
	genericLabels := buildPostgresGenericMetricLabels(cr, instance, clusterID)

	// test the connection
	conn := p.TCPPinger.TCPConnection(postgresInstance.Host, postgresInstance.Port)
	if !conn {
		// create failed connection metric
		resources.SetMetric(resources.DefaultPostgresConnectionMetricName, genericLabels, 0)
		return
	}
	// create successful connection metric
	resources.SetMetric(resources.DefaultPostgresConnectionMetricName, genericLabels, 1)

}
