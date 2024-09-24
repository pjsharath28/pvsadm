// Copyright 2021 IBM Corp
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package _import

import (
	"fmt"
	"strings"
	"time"

	pmodels "github.com/IBM-Cloud/power-go-client/power/models"
	"github.com/IBM/platform-services-go-sdk/resourcecontrollerv2"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/ppc64le-cloud/pvsadm/pkg"
	"github.com/ppc64le-cloud/pvsadm/pkg/client"
	"github.com/ppc64le-cloud/pvsadm/pkg/utils"
)

const (
	accessKeyId          = "access_key_id"
	cosHmacKeys          = "cos_hmac_keys"
	crnServiceRoleWriter = "crn:v1:bluemix:public:iam::::serviceRole:Writer"
	imageStateActive     = "active"
	jobStateCompleted    = "completed"
	jobStateFailed       = "failed"
	secretAccessKey      = "secret_access_key"
	// CosResourceID is IBM COS service id, can be retrieved using ibmcloud cli
	// ibmcloud catalog service cloud-object-storage.
	serviceCredPrefix = "pvsadm-service-cred"
)

// findCOSInstance retrieves the service instance in which the bucket is present.
func findCOSInstanceDetails(resources []resourcecontrollerv2.ResourceInstance, pvsClient *client.Client) *resourcecontrollerv2.ResourceInstance {
	for _, resource := range resources {
		s3client, err := client.NewS3Client(pvsClient, *resource.Name, pkg.ImageCMDOptions.Region)
		if err != nil {
			klog.Warningf("cannot create a new s3 client. err: %v", err)
			continue
		}
		buckets, err := s3client.S3Session.ListBuckets(nil)
		if err != nil {
			klog.Warningf("cannot list buckets in the resource instance. err: %v", err)
			continue
		}
		for _, bucket := range buckets.Buckets {
			if *bucket.Name == pkg.ImageCMDOptions.BucketName {
				return &resource
			}
		}
	}
	return nil
}

// createNewCredentialsWithHMAC generates the service credentials in the given COS instance with HMAC keys.
func createNewCredentialsWithHMAC(pvsClient *client.Client, cosCRN, serviceCredName string) (*resourcecontrollerv2.ResourceKey, error) {
	klog.V(2).Infof("Auto generating COS service credentials to import image: %s", serviceCredName)
	params := &resourcecontrollerv2.ResourceKeyPostParameters{}
	params.SetProperty("HMAC", true)
	key, _, err := pvsClient.ResourceControllerClient.CreateResourceKey(
		&resourcecontrollerv2.CreateResourceKeyOptions{
			Name:       ptr.To(serviceCredName),
			Parameters: params,
			Role:       ptr.To(crnServiceRoleWriter),
			Source:     ptr.To(cosCRN),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create resource key for service instance: %v", err.Error())
	}
	return key, nil
}

// checkStorageTierAvailability confirms if the provided cloud instance ID supports the required storageType.
func checkStorageTierAvailability(pvsClient *client.PVMClient, storageType string) error {
	// Supported tiers are Tier0, Tier1, Tier3 and Tier 5k
	// The use of fixed IOPS is limited to volumes with a size of 200 GB or less, which is the break even size with Tier 0
	// (200 GB @ 25 IOPS/GB = 5000 IOPS).
	// Ref: https://cloud.ibm.com/docs/power-iaas?topic=power-iaas-on-cloud-architecture#storage-tiers
	// API Docs for Storagetypes: https://cloud.ibm.com/docs/power-iaas?topic=power-iaas-on-cloud-architecture#IOPS-api

	validStorageType := []string{"tier3", "tier1", "tier0", "tier5k"}
	if !utils.Contains(validStorageType, storageType) {
		return fmt.Errorf("provide valid StorageType. Allowable values are %v", validStorageType)
	}

	storageTiers, err := pvsClient.StorageTierClient.GetAll()
	if err != nil {
		return fmt.Errorf("an error occured while retriving the Storage tier availability. err:%v", err)
	}
	for _, storageTier := range storageTiers {
		if storageTier.Name == storageType && *storageTier.State == "inactive" {
			return fmt.Errorf("the requested storage tier is not available in the provided cloud instance. Please retry with a different tier")
		}
	}
	return nil
}

var Cmd = &cobra.Command{
	Use:   "import",
	Short: "Import the image into PowerVS workpace",
	Long: `Import the image into PowerVS workpace
pvsadm image import --help for information

# Set the API key or feed the --api-key commandline argument
export IBMCLOUD_API_KEY=<IBM_CLOUD_API_KEY>

# To Import the image across the two different IBM account use "--accesskey" and "--secretkey" options

# To Import the image from public bucket use the "--public-bucket" or "-p" option

Examples:

# import image using default storage type (service credential will be autogenerated)
pvsadm image import -n upstream-core-lon04 -b <BUCKETNAME> --object rhel-83-10032020.ova.gz --pvs-image-name test-image -r <REGION>

# import image using default storage type with specifying the accesskey and secretkey explicitly
pvsadm image import -n upstream-core-lon04 -b <BUCKETNAME> --accesskey <ACCESSKEY> --secretkey <SECRETKEY> --object rhel-83-10032020.ova.gz --pvs-image-name test-image -r <REGION>

# with user provided storage type
pvsadm image import -n upstream-core-lon04 -b <BUCKETNAME> --pvs-storagetype <STORAGETYPE> --object rhel-83-10032020.ova.gz --pvs-image-name test-image -r <REGION>

# If user wants to specify the type of OS
pvsadm image import -n upstream-core-lon04 -b <BUCKETNAME> --object rhel-83-10032020.ova.gz --pvs-image-name test-image -r <REGION>

# import image from a public IBM Cloud Storage bucket
pvsadm image import -n upstream-core-lon04 -b <BUCKETNAME> --object rhel-83-10032020.ova.gz --pvs-image-name test-image -r <REGION> --public-bucket
`,
	PreRunE: func(cmd *cobra.Command, args []string) error {
		// ensure that both, the AccessKey and SecretKey are either both set or unset
		if (len(pkg.ImageCMDOptions.AccessKey) > 0) != (len(pkg.ImageCMDOptions.SecretKey) > 0) {
			return fmt.Errorf("required both --accesskey and --secretkey values")
		}
		return utils.EnsurePrerequisitesAreSet(pkg.Options.APIKey, pkg.ImageCMDOptions.WorkspaceID, pkg.ImageCMDOptions.WorkspaceName)
	},

	RunE: func(cmd *cobra.Command, args []string) error {
		opt := pkg.ImageCMDOptions

		pvsClient, err := client.NewClientWithEnv(pkg.Options.APIKey, pkg.Options.Environment, pkg.Options.Debug)
		if err != nil {
			return err
		}

		pvmclient, err := client.NewPVMClientWithEnv(pvsClient, opt.WorkspaceID, opt.WorkspaceName, pkg.Options.Environment)
		if err != nil {
			return err
		}

		if err := checkStorageTierAvailability(pvmclient, opt.StorageType); err != nil {
			return err
		}

		//Create AccessKey and SecretKey for the bucket provided if bucket access is private
		if (opt.AccessKey == "" || opt.SecretKey == "") && (!opt.Public) {
			// Find COS instance of the bucket
			listServiceInstanceOptions := &resourcecontrollerv2.ListResourceInstancesOptions{
				ResourceID: ptr.To(utils.CosResourceID),
			}

			workspaces, _, err := pvsClient.ResourceControllerClient.ListResourceInstances(listServiceInstanceOptions)
			if err != nil {
				return fmt.Errorf("failed to list the resource instances: %v", err)
			}
			if len(workspaces.Resources) == 0 {
				return fmt.Errorf("no service instances were found")
			}

			cosInstance := findCOSInstanceDetails(workspaces.Resources, pvsClient)
			if cosInstance == nil {
				return fmt.Errorf("failed to find the COS instance for the bucket mentioned: %s", opt.BucketName)
			}

			klog.Infof("Identified bucket %q in service instance: %s", opt.BucketName, *cosInstance.Name)
			listResourceKeysInstanceOptions := &resourcecontrollerv2.ListResourceKeysForInstanceOptions{
				ID: cosInstance.GUID,
			}
			keys, _, err := pvsClient.ResourceControllerClient.ListResourceKeysForInstance(listResourceKeysInstanceOptions)
			if err != nil {
				return fmt.Errorf("cannot list the resource keys for instance. err: %v", err)
			}

			var ok, credentialsPresent bool
			var hmacKeys map[string]interface{}
			var key *resourcecontrollerv2.ResourceKey

			if opt.ServiceCredName == "" {
				opt.ServiceCredName = serviceCredPrefix + "-" + *cosInstance.Name
			}

			// Create the service credential if does not exist
			if len(keys.Resources) == 0 {
				if key, err = createNewCredentialsWithHMAC(pvsClient, *cosInstance.CRN, opt.ServiceCredName); err != nil {
					return fmt.Errorf("error while creating HMAC credentials. err: %v", err)
				}
			} else {
				klog.V(2).Info("Reading the existing service credential")
				// Use the service credential already created. There may be a possibility that multiple credentials exist, but the HMAC credentials may not be present.
				// In such case, manually re-create the credentials.

				for _, serviceCredential := range keys.Resources {
					key, _, err = pvsClient.ResourceControllerClient.GetResourceKey(
						&resourcecontrollerv2.GetResourceKeyOptions{
							ID: serviceCredential.ID,
						},
					)
					if err != nil {
						return fmt.Errorf("an error occured while retriving the resource key. err: %v", err)
					}
					// if the current credential has COS HMAC keys, reuse the same for importing the image
					if prop := key.Credentials.GetProperty(cosHmacKeys); prop != nil {
						klog.Infof("HMAC keys are available from the credential %q, re-using the same for image upload", *key.Name)
						credentialsPresent = true
						break
					}
					klog.Infof("No credentials found in the key %q.", *key.Name)
				}
				// if all the available service credentials do not have HMAC, create one with HMAC.
				if !credentialsPresent {
					if key, err = createNewCredentialsWithHMAC(pvsClient, *cosInstance.CRN, opt.ServiceCredName); err != nil {
						return fmt.Errorf("error while creating HMAC credentials. err: %v", err)
					}
				}
			}

			prop := key.Credentials.GetProperty(cosHmacKeys)
			if prop == nil {
				return fmt.Errorf("unable to retrieve COS HMAC keys")
			}

			if hmacKeys, ok = prop.(map[string]interface{}); !ok {
				return fmt.Errorf("type assertion for HMAC keys failed")
			}
			// Assign the Access Key and Secret Key for further operation
			opt.AccessKey = hmacKeys[accessKeyId].(string)
			opt.SecretKey = hmacKeys[secretAccessKey].(string)
		}

		//By default Bucket Access is private
		bucketAccess := "private"

		if opt.Public {
			bucketAccess = "public"
		}
		klog.Infof("Importing image %s. Please wait...", opt.ImageName)
		jobRef, err := pvmclient.ImgClient.ImportImage(opt.ImageName, opt.ImageFilename, opt.Region,
			opt.AccessKey, opt.SecretKey, opt.BucketName, strings.ToLower(opt.StorageType), bucketAccess)
		if err != nil {
			return err
		}
		start := time.Now()
		err = utils.PollUntil(time.Tick(2*time.Minute), time.After(opt.WatchTimeout), func() (bool, error) {
			job, err := pvmclient.JobClient.Get(*jobRef.ID)
			if err != nil {
				return false, fmt.Errorf("image import job failed to complete, err: %v", err)
			}
			if *job.Status.State == jobStateCompleted {
				klog.V(2).Infof("Image uploaded successfully, took %s", time.Since(start).Round(time.Second))
				return true, nil
			}
			if *job.Status.State == jobStateFailed {
				return false, fmt.Errorf("image import job failed to complete, err: %v", job.Status.Message)
			}
			klog.Infof("Image import is in-progress, current state: %s", *job.Status.State)
			return false, nil
		})
		if err != nil {
			return err
		}

		var image = &pmodels.ImageReference{}
		klog.Info("Retrieving image details")

		if image.ImageID == nil {
			image, err = pvmclient.ImgClient.GetImageByName(opt.ImageName)
			if err != nil {
				return err
			}
		}

		if !opt.Watch {
			klog.Infof("Image import for %s is currently in %s state, Please check the progress in the IBM cloud UI", *image.Name, *image.State)
			return nil
		}
		klog.Infof("Waiting for image %s to be active. Please wait...", opt.ImageName)
		return utils.PollUntil(time.Tick(10*time.Second), time.After(opt.WatchTimeout), func() (bool, error) {
			img, err := pvmclient.ImgClient.Get(*image.ImageID)
			if err != nil {
				return false, fmt.Errorf("failed to import the image, err: %v\n\nRun the command \"pvsadm get events -i %s\" to get more information about the failure", err, pvmclient.InstanceID)
			}
			if img.State == imageStateActive {
				klog.Infof("Successfully imported the image: %s with ID: %s Total time taken: %s", *image.Name, *image.ImageID, time.Since(start).Round(time.Second))
				return true, nil
			}
			klog.Infof("Waiting for image to be active. Current state: %s", img.State)
			return false, nil
		})
	},
}

func init() {
	// TODO pvs-instance-name and pvs-instance-id is deprecated and will be removed in a future release
	Cmd.Flags().StringVarP(&pkg.ImageCMDOptions.WorkspaceName, "pvs-instance-name", "n", "", "PowerVS Instance name.")
	Cmd.Flags().MarkDeprecated("pvs-instance-name", "pvs-instance-name is deprecated, workspace-name should be used")
	Cmd.Flags().StringVarP(&pkg.ImageCMDOptions.WorkspaceID, "pvs-instance-id", "i", "", "PowerVS Instance ID.")
	Cmd.Flags().MarkDeprecated("pvs-instance-id", "pvs-instance-id is deprecated, workspace-id should be used")
	Cmd.Flags().StringVarP(&pkg.ImageCMDOptions.WorkspaceName, "workspace-name", "", "", "PowerVS Workspace name.")
	Cmd.Flags().StringVarP(&pkg.ImageCMDOptions.WorkspaceID, "workspace-id", "", "", "PowerVS Workspace ID.")
	Cmd.Flags().StringVarP(&pkg.ImageCMDOptions.BucketName, "bucket", "b", "", "Cloud Object Storage bucket name.")
	Cmd.Flags().StringVarP(&pkg.ImageCMDOptions.COSInstanceName, "cos-instance-name", "s", "", "Cloud Object Storage instance name.")
	// TODO It's deprecated and will be removed in a future release
	Cmd.Flags().MarkDeprecated("cos-instance-name", "will be removed in a future version.")
	Cmd.Flags().StringVarP(&pkg.ImageCMDOptions.Region, "bucket-region", "r", "", "Cloud Object Storage bucket location.")
	Cmd.Flags().StringVarP(&pkg.ImageCMDOptions.ImageFilename, "object", "o", "", "Cloud Object Storage object name.")
	Cmd.Flags().StringVar(&pkg.ImageCMDOptions.AccessKey, "accesskey", "", "Cloud Object Storage HMAC access key.")
	Cmd.Flags().StringVar(&pkg.ImageCMDOptions.SecretKey, "secretkey", "", "Cloud Object Storage HMAC secret key.")
	Cmd.Flags().StringVar(&pkg.ImageCMDOptions.ImageName, "pvs-image-name", "", "Name to PowerVS imported image.")
	Cmd.Flags().BoolVarP(&pkg.ImageCMDOptions.Public, "public-bucket", "p", false, "Cloud Object Storage public bucket.")
	Cmd.Flags().BoolVarP(&pkg.ImageCMDOptions.Watch, "watch", "w", false, "After image import watch for image to be published and ready to use")
	Cmd.Flags().DurationVar(&pkg.ImageCMDOptions.WatchTimeout, "watch-timeout", 1*time.Hour, "watch timeout")
	Cmd.Flags().StringVar(&pkg.ImageCMDOptions.StorageType, "pvs-storagetype", "tier3", `PowerVS Storage type, accepted values are [tier1, tier3, tier0, tier5k].
																						Tier 0            | 25 IOPS/GB
																						Tier 1            | 10 IOPS/GB
																						Tier 3            | 3 IOPS/GB
																						Fixed IOPS/Tier5k |	5000 IOPS regardless of size
																						Note: The use of fixed IOPS is limited to volumes with a size of 200 GB or less, which is the break even size with Tier 0 (200 GB @ 25 IOPS/GB = 5000 IOPS).`)
	Cmd.Flags().StringVar(&pkg.ImageCMDOptions.ServiceCredName, "cos-service-cred", "", "IBM COS Service Credential name to be auto generated(default \""+serviceCredPrefix+"-<COS Name>\")")

	_ = Cmd.MarkFlagRequired("bucket")
	_ = Cmd.MarkFlagRequired("bucket-region")
	_ = Cmd.MarkFlagRequired("pvs-image-name")
	_ = Cmd.MarkFlagRequired("object")
	Cmd.Flags().SortFlags = false
}
