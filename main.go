package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/crossplane/function-sdk-go/errors"
	"github.com/crossplane/function-sdk-go/logging"
	fnv1beta1 "github.com/crossplane/function-sdk-go/proto/v1beta1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/response"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultVaultAddress = "http://vault.vault-system:8200"
	defaultVaultMount   = "secret"

	vaultTokenSecret    = "vault-root-token"
	vaultTokenNamespace = "vault-system"
	
	tlsCertDir = "/tls/server"
)

// Function implements the Crossplane Function gRPC service
type Function struct {
	fnv1beta1.UnimplementedFunctionRunnerServiceServer
	log       logging.Logger
	k8sClient client.Client
	clientset *kubernetes.Clientset
}

// Parameters defines the input schema for this function
type Parameters struct {
	Spec ParametersSpec `json:"spec,omitempty"`
}

type ParametersSpec struct {
	VaultAddress string `json:"vaultAddress,omitempty"`
	VaultMount   string `json:"vaultMount,omitempty"`
}

// VaultPayload is the structure sent to Vault
type VaultPayload struct {
	Data map[string]string `json:"data"`
}

// RunFunction implements the function logic
func (f *Function) RunFunction(ctx context.Context, req *fnv1beta1.RunFunctionRequest) (*fnv1beta1.RunFunctionResponse, error) {
	log.Println("[DEBUG] RunFunction called")
	f.log.Info("RunFunction called")

	rsp := response.To(req, response.DefaultTTL)

	// 1. Get composite resource (AppBackupBucket)
	oxr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get observed composite resource"))
		return rsp, nil
	}

	appName := oxr.Resource.GetName()
	if appName == "" {
		log.Println("[ERROR] Composite resource has no name")
		response.Fatal(rsp, errors.New("composite resource has no name"))
		return rsp, nil
	}

	log.Printf("[DEBUG] Processing AppBackupBucket: %s\n", appName)
	f.log.Info("Processing AppBackupBucket", "app", appName)

	// 2. Parse function input parameters
	params := &Parameters{
		Spec: ParametersSpec{
			VaultAddress: defaultVaultAddress,
			VaultMount:   defaultVaultMount,
		},
	}

	if req.Input != nil {
		inputBytes, err := json.Marshal(req.Input.AsMap())
		if err == nil {
			if err := json.Unmarshal(inputBytes, params); err != nil {
				f.log.Debug("Using default Vault parameters", "error", err.Error())
			}
		}
	}

	// 3. Get observed composed resources to find the ApiKey
	observed, err := request.GetObservedComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get observed composed resources"))
		return rsp, nil
	}

	// Find the ApiKey resource and extract accessKey
	var accessKey string
	log.Printf("[DEBUG] Searching for ApiKey in %d observed resources\n", len(observed))

	for name, res := range observed {
		if res.Resource.GetKind() == "ApiKey" {
			log.Printf("[DEBUG] Found ApiKey resource: %s\n", name)
			f.log.Debug("Found ApiKey resource", "name", name)
			accessKey, err = res.Resource.GetString("status.atProvider.accessKey")
			if err != nil {
				log.Printf("[DEBUG] ApiKey not ready yet: %v\n", err)
				f.log.Debug("ApiKey not ready yet", "error", err.Error())
				// ApiKey not ready yet, return and wait for next reconciliation
				return rsp, nil
			}
			break
		}
	}

	if accessKey == "" {
		log.Println("[DEBUG] No ApiKey found, waiting for resources")
		f.log.Info("No ApiKey found yet, waiting for Scaleway resources to be created")
		return rsp, nil
	}

	log.Printf("[DEBUG] Found access key: %s...\n", accessKey[:8])
	f.log.Info("Found access key", "accessKey", accessKey[:8]+"...")

	// 4. Get secret key from Kubernetes Secret
	// The secret name is constructed as: {appName}-s3-credentials-temp
	secretName := fmt.Sprintf("%s-s3-credentials-temp", appName)
	log.Printf("[DEBUG] Looking for secret: %s in crossplane-system\n", secretName)

	secret := &corev1.Secret{}
	err = f.k8sClient.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: "crossplane-system",
	}, secret)
	if err != nil {
		log.Printf("[DEBUG] Secret not ready: %v\n", err)
		f.log.Debug("Secret not ready yet", "secret", secretName, "error", err.Error())
		return rsp, nil
	}

	secretKeyBytes, ok := secret.Data["attribute.secret_key"]
	if !ok {
		log.Printf("[ERROR] Secret %s does not contain attribute.secret_key\n", secretName)
		response.Fatal(rsp, errors.Errorf("secret %s does not contain attribute.secret_key", secretName))
		return rsp, nil
	}

	secretKey := string(secretKeyBytes)
	log.Println("[DEBUG] Retrieved secret key from Kubernetes Secret")
	f.log.Info("Retrieved secret key from Kubernetes Secret")

	// 5. Get Vault token from vault-root-token secret
	vaultTokenSecret := &corev1.Secret{}
	err = f.k8sClient.Get(ctx, types.NamespacedName{
		Name:      "vault-root-token",
		Namespace: "vault-system",
	}, vaultTokenSecret)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Vault token secret"))
		return rsp, nil
	}

	vaultToken := string(vaultTokenSecret.Data["token"])
	if vaultToken == "" {
		response.Fatal(rsp, errors.New("vault token is empty"))
		return rsp, nil
	}

	f.log.Info("Retrieved Vault token")

	// 6. Prepare Vault payload
	payload := &VaultPayload{
		Data: map[string]string{
			"access_key": accessKey,
			"secret_key": secretKey,
			"bucket":     "bigap-backups",
			"region":     "fr-par",
			"endpoint":   "s3.fr-par.scw.cloud",
		},
	}

	// 7. Push to Vault
	vaultPath := fmt.Sprintf("%s/%s/s3-backup", params.Spec.VaultMount, appName)
	log.Printf("[DEBUG] Pushing to Vault: %s at %s\n", vaultPath, params.Spec.VaultAddress)

	err = f.pushToVault(ctx, params.Spec.VaultAddress, vaultPath, vaultToken, payload)
	if err != nil {
		log.Printf("[ERROR] Failed to push to Vault: %v\n", err)
		response.Fatal(rsp, errors.Wrapf(err, "failed to push to Vault"))
		return rsp, nil
	}

	log.Printf("[SUCCESS] Synced credentials to Vault at %s\n", vaultPath)
	f.log.Info("Successfully synced credentials to Vault", "path", vaultPath)

	return rsp, nil
}

// pushToVault sends credentials to Vault KV v2
func (f *Function) pushToVault(ctx context.Context, vaultAddr, path, token string, payload *VaultPayload) error {
	// Vault KV v2 API path: /v1/{mount}/data/{path}
	url := fmt.Sprintf("%s/v1/%s", vaultAddr, path)

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return errors.Wrap(err, "cannot marshal payload")
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(payloadBytes)))
	if err != nil {
		return errors.Wrap(err, "cannot create HTTP request")
	}

	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return errors.Wrap(err, "cannot send HTTP request to Vault")
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return errors.Errorf("Vault returned status %d: %s", resp.StatusCode, string(body))
	}

	f.log.Info("Successfully pushed to Vault", "url", url, "status", resp.StatusCode)
	return nil
}

// setupKubernetesClient creates a Kubernetes client
func setupKubernetesClient() (client.Client, *kubernetes.Clientset, error) {
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = os.Getenv("HOME") + "/.kube/config"
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, nil, errors.Wrap(err, "cannot create Kubernetes config")
		}
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, nil, errors.Wrap(err, "cannot add core/v1 to scheme")
	}

	k8sClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot create Kubernetes client")
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot create Kubernetes clientset")
	}

	return k8sClient, clientset, nil
}

func main() {
	// Setup logging using controller-runtime
	logger := ctrl.Log.WithName("function-vault-sync")
	funcLog := logging.NewLogrLogger(logger)

	// Setup Kubernetes client
	k8sClient, clientset, err := setupKubernetesClient()
	if err != nil {
		log.Fatalf("Failed to setup Kubernetes client: %v", err)
	}

	// Create and start the gRPC server
	function := &Function{
		log:       funcLog,
		k8sClient: k8sClient,
		clientset: clientset,
	}

	// Load TLS certificates
	certFile := tlsCertDir + "/tls.crt"
	keyFile := tlsCertDir + "/tls.key"
	
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalf("Failed to load TLS certificates: %v", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.NoClientCert,
	}

	creds := credentials.NewTLS(tlsConfig)

	server := grpc.NewServer(
		grpc.Creds(creds),
	)

	fnv1beta1.RegisterFunctionRunnerServiceServer(server, function)

	// Create TCP listener on port 9443
	listener, err := net.Listen("tcp", ":9443")
	if err != nil {
		log.Fatalf("Failed to listen on port 9443: %v", err)
	}

	log.Println("Starting function-vault-sync gRPC server with TLS on port 9443")
	if err := server.Serve(listener); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
