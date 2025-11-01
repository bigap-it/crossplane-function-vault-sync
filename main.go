package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/crossplane/function-sdk-go/errors"
	"github.com/crossplane/function-sdk-go/logging"
	fnv1beta1 "github.com/crossplane/function-sdk-go/proto/v1beta1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
	secretNamespace     = "crossplane-system"
	vaultTokenSecret    = "vault-root-token"
	vaultTokenNamespace = "vault-system"
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
	VaultAddress string `json:"vaultAddress,omitempty"`
	VaultMount   string `json:"vaultMount,omitempty"`
}

// VaultPayload is the structure sent to Vault
type VaultPayload struct {
	Data map[string]string `json:"data"`
}

// RunFunction executes the function logic
func (f *Function) RunFunction(ctx context.Context, req *fnv1beta1.RunFunctionRequest) (*fnv1beta1.RunFunctionResponse, error) {
	f.log.Info("Running Vault sync function")

	rsp := response.To(req, response.DefaultTTL)

	// 1. Get the observed composite resource (AppBackupBucket)
	oxr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get observed composite resource"))
		return rsp, nil
	}

	appName, err := oxr.Resource.GetString("metadata.name")
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get app name from composite"))
		return rsp, nil
	}

	f.log.Info("Processing AppBackupBucket", "app", appName)

	// 2. Parse function input parameters
	params := &Parameters{
		VaultAddress: defaultVaultAddress,
		VaultMount:   defaultVaultMount,
	}

	if req.Input != nil {
		input := &resource.Unstructured{}
		if err := input.From(req.Input); err == nil {
			if addr, err := input.GetString("spec.vaultAddress"); err == nil && addr != "" {
				params.VaultAddress = addr
			}
			if mount, err := input.GetString("spec.vaultMount"); err == nil && mount != "" {
				params.VaultMount = mount
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
	apiKeyName := fmt.Sprintf("%s-s3-apikey", appName)

	for name, res := range observed {
		if res.Resource.GetKind() == "ApiKey" {
			f.log.Debug("Found ApiKey resource", "name", name)
			accessKey, err = res.Resource.GetString("status.atProvider.accessKey")
			if err != nil {
				f.log.Debug("ApiKey not ready yet", "error", err.Error())
				// ApiKey not ready yet, return and wait for next reconciliation
				return rsp, nil
			}
			break
		}
	}

	if accessKey == "" {
		f.log.Info("ApiKey accessKey not available yet, waiting...")
		return rsp, nil
	}

	f.log.Info("Found accessKey", "accessKey", accessKey[:8]+"...")

	// 4. Read the Kubernetes Secret containing the secret key
	secretName := fmt.Sprintf("%s-s3-credentials-temp", appName)
	secret := &corev1.Secret{}

	err = f.k8sClient.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: secretNamespace,
	}, secret)

	if err != nil {
		f.log.Info("Secret not ready yet", "secret", secretName, "error", err.Error())
		return rsp, nil
	}

	// Extract secret_key from the secret
	secretKeyB64, ok := secret.Data["attribute.secret_key"]
	if !ok {
		response.Fatal(rsp, errors.New("attribute.secret_key not found in secret"))
		return rsp, nil
	}

	secretKey := string(secretKeyB64)
	f.log.Info("Found secretKey", "length", len(secretKey))

	// 5. Get Vault token
	vaultToken, err := f.getVaultToken(ctx)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Vault token"))
		return rsp, nil
	}

	// 6. Prepare payload for Vault
	bucket, _ := oxr.Resource.GetString("spec.bucket")
	if bucket == "" {
		bucket = "bigap-backups"
	}

	region, _ := oxr.Resource.GetString("spec.region")
	if region == "" {
		region = "fr-par"
	}

	payload := VaultPayload{
		Data: map[string]string{
			"access_key_id":     accessKey,
			"secret_access_key": secretKey,
			"endpoint":          fmt.Sprintf("https://s3.%s.scw.cloud", region),
			"region":            region,
			"bucket":            bucket,
			"s3_prefix":         appName + "/",
		},
	}

	// 7. Push to Vault
	vaultPath := fmt.Sprintf("%s/%s/s3-backup", params.VaultMount, appName)
	err = f.pushToVault(ctx, params.VaultAddress, vaultPath, vaultToken, payload)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "failed to push to Vault"))
		return rsp, nil
	}

	f.log.Info("Successfully synced credentials to Vault", "path", vaultPath)

	// 8. Update the composite resource status
	dxr, err := request.GetDesiredCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get desired composite resource"))
		return rsp, nil
	}

	// Update status fields
	_ = dxr.Resource.SetString("status.vaultSyncStatus", "synced")
	_ = dxr.Resource.SetString("status.lastSyncTime", time.Now().UTC().Format(time.RFC3339))
	_ = dxr.Resource.SetString("status.vaultPath", vaultPath)

	if err := response.SetDesiredCompositeResource(rsp, dxr); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot set desired composite resource"))
		return rsp, nil
	}

	return rsp, nil
}

// getVaultToken retrieves the Vault root token from Kubernetes
func (f *Function) getVaultToken(ctx context.Context) (string, error) {
	secret := &corev1.Secret{}
	err := f.k8sClient.Get(ctx, types.NamespacedName{
		Name:      vaultTokenSecret,
		Namespace: vaultTokenNamespace,
	}, secret)

	if err != nil {
		return "", errors.Wrapf(err, "cannot get vault token secret")
	}

	tokenB64, ok := secret.Data["token"]
	if !ok {
		return "", errors.New("token key not found in vault secret")
	}

	// Decode if it's base64 encoded
	token, err := base64.StdEncoding.DecodeString(string(tokenB64))
	if err != nil {
		// If decode fails, assume it's already plain text
		return string(tokenB64), nil
	}

	return string(token), nil
}

// pushToVault sends credentials to Vault via HTTP API
func (f *Function) pushToVault(ctx context.Context, vaultAddr, path, token string, payload VaultPayload) error {
	url := fmt.Sprintf("%s/v1/%s", strings.TrimSuffix(vaultAddr, "/"), path)

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return errors.Wrapf(err, "cannot marshal payload")
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(jsonData)))
	if err != nil {
		return errors.Wrapf(err, "cannot create HTTP request")
	}

	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return errors.Wrapf(err, "HTTP request failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return errors.Errorf("Vault returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	f.log.Info("Successfully pushed to Vault", "url", url, "status", resp.StatusCode)
	return nil
}

// setupKubernetesClient creates a Kubernetes client
func setupKubernetesClient() (client.Client, *kubernetes.Clientset, error) {
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err != nil {
		// Fallback to kubeconfig for local development
		config, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
		if err != nil {
			return nil, nil, errors.Wrap(err, "cannot create kubernetes config")
		}
	}

	// Create controller-runtime client
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	k8sClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot create kubernetes client")
	}

	// Create clientset for additional operations
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot create kubernetes clientset")
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

	server := grpc.NewServer(
		grpc.Creds(insecure.NewCredentials()),
	)

	fnv1beta1.RegisterFunctionRunnerServiceServer(server, function)

	log.Println("Starting function-vault-sync gRPC server on port 9443")
	if err := server.Serve(nil); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
