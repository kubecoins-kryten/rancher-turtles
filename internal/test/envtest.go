package test

import (
	"errors"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
}

// StartEnvTest starts a new test environment.
func StartEnvTest(testEnv *envtest.Environment) (*rest.Config, client.Client, error) {
	testEnv.CRDs = []*apiextensionsv1.CustomResourceDefinition{
		fakeRancherClusterCRD,
		fakeRegistrationTokenCRD,
		fakeCAPIClusterCRD,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		return nil, nil, err
	}

	if cfg == nil {
		return nil, nil, errors.New("envtest.Environment.Start() returned nil config")
	}

	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, err
	}

	return cfg, cl, nil
}

// StopEnvTest stops the test environment.
func StopEnvTest(testEnv *envtest.Environment) error {
	return testEnv.Stop()
}
