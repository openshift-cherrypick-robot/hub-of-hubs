// Copyright (c) 2022 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package dbsyncer_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/confluentinc/confluent-kafka-go/kafka"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	managerscheme "github.com/stolostron/multicluster-global-hub/manager/pkg/scheme"
	"github.com/stolostron/multicluster-global-hub/manager/pkg/specsyncer/db2transport/db/postgresql"
	specsycner "github.com/stolostron/multicluster-global-hub/manager/pkg/specsyncer/db2transport/syncer"
	"github.com/stolostron/multicluster-global-hub/pkg/bundle/registration"
	specbundle "github.com/stolostron/multicluster-global-hub/pkg/bundle/spec"
	"github.com/stolostron/multicluster-global-hub/pkg/compressor"
	"github.com/stolostron/multicluster-global-hub/pkg/constants"
	"github.com/stolostron/multicluster-global-hub/pkg/transport/consumer"
	"github.com/stolostron/multicluster-global-hub/pkg/transport/producer"
	"github.com/stolostron/multicluster-global-hub/test/pkg/testpostgres"
)

var (
	testenv                 *envtest.Environment
	cfg                     *rest.Config
	ctx                     context.Context
	cancel                  context.CancelFunc
	mgr                     ctrl.Manager
	kubeClient              client.Client
	testPostgres            *testpostgres.TestPostgres
	transportPostgreSQL     *postgresql.PostgreSQL
	kafkaConsumer           *consumer.KafkaConsumer
	kafkaProducer           *producer.KafkaProducer
	mockCluster             *kafka.MockCluster
	customBundleUpdatesChan = make(chan interface{})
)

func TestSpecSyncer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Spec Syncer Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	Expect(os.Setenv("POD_NAMESPACE", "default")).To(Succeed())

	ctx, cancel = context.WithCancel(context.Background())

	By("Prepare envtest environment")
	var err error
	testenv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "..", "..", "..", "pkg", "testdata", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err = testenv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	By("Create test postgres")
	testPostgres, err = testpostgres.NewTestPostgres()
	Expect(err).NotTo(HaveOccurred())
	transportPostgreSQL, err = postgresql.NewPostgreSQL(testPostgres.URI)
	Expect(err).NotTo(HaveOccurred())

	By("Create mock kafka cluster")
	mockCluster, err = kafka.NewMockCluster(1)
	Expect(err).NotTo(HaveOccurred())
	fmt.Fprintf(GinkgoWriter, "mock kafka bootstrap server address: %s\n", mockCluster.BootstrapServers())

	By("Start kafka producer")
	kafkaProducerConfig := &producer.KafkaProducerConfig{
		ProducerTopic:  "spec",
		ProducerID:     "spec-producer",
		MsgSizeLimitKB: 1,
	}
	kafkaProducer, err = producer.NewKafkaProducer(&compressor.CompressorGZip{},
		mockCluster.BootstrapServers(), "", kafkaProducerConfig,
		ctrl.Log.WithName("kafka-producer"))
	Expect(err).NotTo(HaveOccurred())

	By("Start kafka consumer")
	kafkaConsumerConfig := &consumer.KafkaConsumerConfig{
		ConsumerTopic: "spec",
		ConsumerID:    "spec-consumer",
	}
	kafkaConsumer, err = consumer.NewKafkaConsumer(
		mockCluster.BootstrapServers(), "", kafkaConsumerConfig,
		ctrl.Log.WithName("kafka-consumer"))
	Expect(err).NotTo(HaveOccurred())

	mgr, err = ctrl.NewManager(cfg, ctrl.Options{
		MetricsBindAddress: "0",
		Scheme:             scheme.Scheme,
	})
	Expect(err).NotTo(HaveOccurred())

	By("Get kubeClient")
	kubeClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(kubeClient).NotTo(BeNil())

	By("Add to Scheme")
	Expect(managerscheme.AddToScheme(mgr.GetScheme())).NotTo(HaveOccurred())

	By("Create the global hub ConfigMap with aggregationLevel=full and enableLocalPolicies=true")
	mghSystemNamespace := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: constants.GHSystemNamespace}}
	Expect(kubeClient.Create(ctx, mghSystemNamespace)).Should(Succeed())
	mghSystemConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      constants.GHConfigCMName,
			Namespace: constants.GHSystemNamespace,
		},
		Data: map[string]string{"aggregationLevel": "full", "enableLocalPolicies": "true"},
	}
	Expect(kubeClient.Create(ctx, mghSystemConfigMap)).Should(Succeed())

	Expect(specsycner.AddDB2TransportSyncers(mgr, transportPostgreSQL, kafkaProducer, 1*time.Second)).Should(Succeed())

	Expect(mgr.Add(kafkaProducer)).Should(Succeed())

	kafkaConsumer.SetLeafHubName(leafhubName)
	// register custom bundle update channel for managed cluster label updates
	kafkaConsumer.CustomBundleRegister(constants.ManagedClustersLabelsMsgKey, &registration.CustomBundleRegistration{
		InitBundlesResourceFunc: func() interface{} {
			return &specbundle.ManagedClusterLabelsSpecBundle{}
		},
		BundleUpdatesChan: customBundleUpdatesChan,
	})
	Expect(mgr.Add(kafkaConsumer)).Should(Succeed())

	By("Start the manager")
	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).ToNot(HaveOccurred(), "failed to run manager")
	}()

	By("Waiting for the manager to be ready")
	Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())
})

var _ = AfterSuite(func() {
	cancel()
	mockCluster.Close()
	transportPostgreSQL.Stop()
	Expect(testPostgres.Stop()).NotTo(HaveOccurred())

	By("Tearing down the test environment")
	err := testenv.Stop()
	// https://github.com/kubernetes-sigs/controller-runtime/issues/1571
	// Set 4 with random
	if err != nil {
		time.Sleep(4 * time.Second)
	}
	Expect(testenv.Stop()).NotTo(HaveOccurred())
})