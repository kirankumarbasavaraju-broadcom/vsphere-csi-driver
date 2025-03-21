/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"os"

	"github.com/go-logr/zapr"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha1"
	vmopv3 "github.com/vmware-tanzu/vm-operator/api/v1alpha3"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	fnodes "k8s.io/kubernetes/test/e2e/framework/node"
	admissionapi "k8s.io/pod-security-admission/api"
	ctlrclient "sigs.k8s.io/controller-runtime/pkg/client"
	cr_log "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/crypto"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
)

var _ = ginkgo.Describe("dev-ops-user-tests", func() {
	f := framework.NewDefaultFramework("devops")
	f.NamespacePodSecurityEnforceLevel = admissionapi.LevelPrivileged

	log := logger.GetLogger(context.Background())
	cr_log.SetLogger(zapr.NewLogger(log.Desugar()))

	var (
		client                clientset.Interface
		cryptoClient          crypto.Client
		vmopClient            ctlrclient.Client
		vmi                   string
		vmClass               string
		namespace             string
		standardStorageClass  *storagev1.StorageClass
		encryptedStorageClass *storagev1.StorageClass
		keyProviderID         string
	)

	ginkgo.BeforeEach(func() {
		bootstrap()
		client = f.ClientSet
		namespace = getNamespaceToRunTests(f)
		restConfig = getRestConfigClient()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		nodeList, err := fnodes.GetReadySchedulableNodes(ctx, f.ClientSet)
		framework.ExpectNoError(err, "Unable to find ready and schedulable Node")
		if !(len(nodeList.Items) > 0) {
			framework.Failf("Unable to find ready and schedulable Node")
		}

		// Init VC client
		err = connectCns(ctx, &e2eVSphere)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Init crypto client
		cryptoClient, err = crypto.NewClientWithConfig(ctx, f.ClientConfig())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Load standard storage class
		standardStoragePolicyName := GetAndExpectStringEnvVar(envStoragePolicyNameForSharedDatastores)
		standardStorageClass, err = createStorageClass(client,
			map[string]string{
				scParamStoragePolicyID: e2eVSphere.GetSpbmPolicyID(standardStoragePolicyName),
			},
			nil, "", "", false, standardStoragePolicyName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(),
			fmt.Sprintf("Failed to create storage class with err: %v", err))
		validateEncryptedStorageClass(ctx, cryptoClient, standardStoragePolicyName, false)

		// Load encrypted storage class
		encryptedStoragePolicyName := GetAndExpectStringEnvVar(envStoragePolicyNameWithEncryption)
		encryptedStorageClass, err = createStorageClass(client,
			map[string]string{
				scParamStoragePolicyID: e2eVSphere.GetSpbmPolicyID(encryptedStoragePolicyName),
			},
			nil, "", "", false, encryptedStoragePolicyName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(),
			fmt.Sprintf("Failed to create storage class with err: %v", err))
		validateEncryptedStorageClass(ctx, cryptoClient, encryptedStoragePolicyName, true)

		// Load key providers
		keyProviderID = GetAndExpectStringEnvVar(envKeyProvider)
		validateKeyProvider(ctx, keyProviderID)

		// Load VM-related properties
		vmClass = os.Getenv(envVMClass)
		if vmClass == "" {
			vmClass = vmClassBestEffortSmall
		}
		vmopScheme := runtime.NewScheme()
		gomega.Expect(vmopv1.AddToScheme(vmopScheme)).Should(gomega.Succeed())
		gomega.Expect(vmopv3.AddToScheme(vmopScheme)).Should(gomega.Succeed())
		vmopClient, err = ctlrclient.New(f.ClientConfig(), ctlrclient.Options{Scheme: vmopScheme})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		vmImageName := GetAndExpectStringEnvVar(envVmsvcVmImageName)
		framework.Logf("Waiting for virtual machine image list to be available in namespace '%s' for image '%s'",
			namespace, vmImageName)
		vmi = waitNGetVmiForImageName(ctx, vmopClient, vmImageName)
		gomega.Expect(vmi).NotTo(gomega.BeEmpty())
	})

	ginkgo.AfterEach(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if vanillaCluster {
			if standardStorageClass != nil {
				err := client.
					StorageV1().
					StorageClasses().
					Delete(ctx, standardStorageClass.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}

			if encryptedStorageClass != nil {
				err := client.
					StorageV1().
					StorageClasses().
					Delete(ctx, encryptedStorageClass.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
		}

		svcClient, svNamespace := getSvcClientAndNamespace()
		dumpSvcNsEventsOnTestFailure(svcClient, svNamespace)
	})

	/*
		Steps:
		1. Generate first encryption key
		2. Create first EncryptionClass with encryption key [1]
		3. As devops user Creating PVC with first EncryptionClass [2]
	*/
	ginkgo.It("[svc-devops-user-test-encryption] As devops user create PVC with EncryptionClass", ginkgo.Label(p1, block, wcp, core, vc90), func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ginkgo.By("1. Generate first encryption key")
		keyID1 := e2eVSphere.generateEncryptionKey(ctx, keyProviderID)

		ginkgo.By("2. Create first EncryptionClass with encryption key [1]")
		encClass1 := createEncryptionClass(ctx, cryptoClient, namespace, keyProviderID, keyID1, false)
		defer deleteEncryptionClass(ctx, cryptoClient, encClass1)

		ginkgo.By("3. Creating PVC with first EncryptionClass [3]")
		var devopsclient clientset.Interface
		var err error
		if k8senvsv := GetAndExpectStringEnvVar("DEV_OPS_USER_KUBECONFIG"); k8senvsv != "" {
			devopsclient, err = createKubernetesClientFromConfig(k8senvsv)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		pvc := createPersistentVolumeClaim(ctx, devopsclient, PersistentVolumeClaimOptions{
			Namespace:           namespace,
			StorageClassName:    encryptedStorageClass.Name,
			EncryptionClassName: encClass1.Name,
		})
		defer deletePersistentVolumeClaim(ctx, client, pvc)
	})

})
