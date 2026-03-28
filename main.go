package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	maasAuthLabel           = "rhai-tmm.dev/maas-auth"
	maasAuthUntilAnnotation = "rhai-tmm.dev/maas-auth-until"
)

var (
	maasURL   string
	maasToken string

	tokenExpiry       string
	reconcileFreq     time.Duration
	chatbotConfigJSON string
)

// parseExpiry parses a duration string like "8h", "30m", or "1d" into a time.Duration.
func parseExpiry(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid day duration %q: %w", s, err)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// computeExpiresAt returns the UTC time when the current token expiry will elapse.
// Falls back to 8h if the expiry string cannot be parsed.
func computeExpiresAt() time.Time {
	d, err := parseExpiry(tokenExpiry)
	if err != nil {
		log.Printf("Warning: cannot parse token-expiry %q, defaulting to 8h: %v", tokenExpiry, err)
		d = 8 * time.Hour
	}
	return time.Now().UTC().Add(d)
}

func main() {
	flag.StringVar(&tokenExpiry, "token-expiry", "8h", "MaaS token expiration duration (e.g. 2h, 1d, 30m)")
	flag.DurationVar(&reconcileFreq, "reconcile-frequency", 10*time.Minute, "How often to reconcile all tenants")
	flag.Parse()

	maasURL = os.Getenv("MAAS_URL")
	maasToken = os.Getenv("MAAS_TOKEN")
	if maasURL == "" || maasToken == "" {
		log.Fatal("MAAS_URL and MAAS_TOKEN environment variables are required")
	}

	chatbotConfigJSON = os.Getenv("CHATBOT_CONFIG")
	if chatbotConfigJSON != "" {
		log.Printf("CHATBOT_CONFIG template loaded for multimodal-chatbot config.json")
	}

	log.Printf("MaaS tokenizer starting (url: %s, token-expiry: %s, reconcile: %s)", maasURL, tokenExpiry, reconcileFreq)

	config, err := buildKubeConfig()
	if err != nil {
		log.Fatalf("Failed to build kubeconfig: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create kubernetes client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("Received signal %v, shutting down", sig)
		cancel()
	}()

	// Initial reconciliation
	reconcileAllTenants(ctx, clientset)

	// Start namespace watcher in background
	go watchNamespaces(ctx, clientset)

	// Periodic reconciliation
	ticker := time.NewTicker(reconcileFreq)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down")
			return
		case <-ticker.C:
			reconcileAllTenants(ctx, clientset)
		}
	}
}

func buildKubeConfig() (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		home, _ := os.UserHomeDir()
		kubeconfigPath = home + "/.kube/config"
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
}

// watchNamespaces watches for new/modified namespaces with tenant labels and
// provisions MaaS secrets immediately, without waiting for the reconcile ticker.
// Automatically reconnects on watch errors.
func watchNamespaces(ctx context.Context, clientset kubernetes.Interface) {
	for {
		if ctx.Err() != nil {
			return
		}
		log.Println("Starting namespace watcher...")
		if err := runNamespaceWatch(ctx, clientset); err != nil {
			log.Printf("Namespace watcher error: %v, reconnecting in 5s...", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func runNamespaceWatch(ctx context.Context, clientset kubernetes.Interface) error {
	watcher, err := clientset.CoreV1().Namespaces().Watch(ctx, metav1.ListOptions{
		LabelSelector: "tenant",
	})
	if err != nil {
		return fmt.Errorf("creating watch: %w", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed")
			}
			if event.Type != watch.Added && event.Type != watch.Modified {
				continue
			}
			ns, ok := event.Object.(*corev1.Namespace)
			if !ok {
				continue
			}

			if isAlreadyProvisioned(ctx, clientset, ns.Name) {
				continue
			}

			log.Printf("[watch] Tenant namespace detected: %s", ns.Name)

			token, models, err := getMaaSTokenAndModels(ctx)
			if err != nil {
				log.Printf("[watch] Error getting MaaS token/models for %s: %v", ns.Name, err)
				continue
			}
			apiBaseURLs, apiKeys := buildModelData(token, models)
			expiresAt := computeExpiresAt()

			if err := provisionNamespace(ctx, clientset, ns.Name, token, models, apiBaseURLs, apiKeys, expiresAt); err != nil {
				log.Printf("[watch] Error provisioning %s: %v", ns.Name, err)
			}
		}
	}
}

func labelNamespaceDone(ctx context.Context, clientset kubernetes.Interface, namespace string, expiresAt time.Time) error {
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if ns.Labels == nil {
		ns.Labels = make(map[string]string)
	}
	ns.Labels[maasAuthLabel] = "done"
	if ns.Annotations == nil {
		ns.Annotations = make(map[string]string)
	}
	ns.Annotations[maasAuthUntilAnnotation] = expiresAt.Format(time.RFC3339)
	if _, err := clientset.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{}); err != nil {
		return err
	}
	log.Printf("Labelled namespace %s with %s=done (until %s)", namespace, maasAuthLabel, expiresAt.Format(time.RFC3339))
	return nil
}

// isAlreadyProvisioned checks if all app namespaces for a tenant are labelled done
// and their MaaS auth has not expired.
func isAlreadyProvisioned(ctx context.Context, clientset kubernetes.Interface, tenantNS string) bool {
	now := time.Now().UTC()
	for _, prefix := range []string{"openwebui-", "multimodal-chat-"} {
		ns, err := clientset.CoreV1().Namespaces().Get(ctx, prefix+tenantNS, metav1.GetOptions{})
		if err != nil || ns.Labels[maasAuthLabel] != "done" {
			return false
		}
		until := ns.Annotations[maasAuthUntilAnnotation]
		if until == "" {
			return false
		}
		expiresAt, err := time.Parse(time.RFC3339, until)
		if err != nil || now.After(expiresAt) {
			return false
		}
	}
	return true
}

// reconcileAllTenants finds all namespaces with the "tenant" label and provisions MaaS secrets
// into their per-app namespaces.
func reconcileAllTenants(ctx context.Context, clientset kubernetes.Interface) {
	log.Println("Reconciling all tenants...")

	namespaces, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: "tenant",
	})
	if err != nil {
		log.Printf("Error listing namespaces: %v", err)
		return
	}

	var unprovisioned []string
	for _, ns := range namespaces.Items {
		if !isAlreadyProvisioned(ctx, clientset, ns.Name) {
			unprovisioned = append(unprovisioned, ns.Name)
		}
	}

	if len(unprovisioned) == 0 {
		log.Println("No unprovisioned tenant namespaces found")
		return
	}

	log.Printf("Found %d unprovisioned tenant namespace(s), obtaining MaaS token...", len(unprovisioned))

	token, models, err := getMaaSTokenAndModels(ctx)
	if err != nil {
		log.Printf("Error getting MaaS token/models: %v", err)
		return
	}

	apiBaseURLs, apiKeys := buildModelData(token, models)
	log.Printf("Found %d model(s): %s", len(models.Data), apiBaseURLs)
	expiresAt := computeExpiresAt()

	for _, nsName := range unprovisioned {
		if err := provisionNamespace(ctx, clientset, nsName, token, models, apiBaseURLs, apiKeys, expiresAt); err != nil {
			log.Printf("[%s] Error provisioning: %v", nsName, err)
		}
	}

	log.Println("Reconciliation complete")
}

// provisionNamespace creates MaaS secrets in per-app namespaces for a tenant and labels each done.
// App namespaces follow the pattern {app-prefix}-{tenantNS}:
//   - openwebui-{tenantNS}:       chat-openwebui ConfigMap + Secret
//   - multimodal-chat-{tenantNS}: multimodal-chatbot Secret
func provisionNamespace(ctx context.Context, clientset kubernetes.Interface, tenantNS, token string, models *maasModelsResponse, apiBaseURLs, apiKeys string, expiresAt time.Time) error {
	managedLabels := map[string]string{
		"app.kubernetes.io/managed-by": "maas-tokenizer",
		"tenant.name":                 tenantNS,
	}

	// --- openwebui-{tenantNS} ---
	openwebuiNS := "openwebui-" + tenantNS
	tenantID := strings.TrimPrefix(tenantNS, "user-")
	openwebuiResourceName := "openwebui-" + tenantID

	if err := upsertConfigMap(ctx, clientset, openwebuiNS, openwebuiResourceName, map[string]string{
		"OPENAI_API_BASE_URLS": apiBaseURLs,
	}, managedLabels); err != nil {
		return fmt.Errorf("%s configmap in %s: %w", openwebuiResourceName, openwebuiNS, err)
	}

	if err := upsertSecret(ctx, clientset, openwebuiNS, openwebuiResourceName, map[string][]byte{
		"OPENAI_API_KEYS": []byte(apiKeys),
	}, managedLabels); err != nil {
		return fmt.Errorf("%s secret in %s: %w", openwebuiResourceName, openwebuiNS, err)
	}

	if err := labelNamespaceDone(ctx, clientset, openwebuiNS, expiresAt); err != nil {
		return fmt.Errorf("labelling namespace %s: %w", openwebuiNS, err)
	}

	restartPods(ctx, clientset, openwebuiNS, "app.kubernetes.io/name=openwebui")

	// --- multimodal-chat-{tenantNS} ---
	multimodalChatNS := "multimodal-chat-" + tenantNS

	if err := upsertMultimodalChatbotSecret(ctx, clientset, tenantNS, multimodalChatNS, token, models, managedLabels); err != nil {
		return fmt.Errorf("multimodal-chatbot secret in %s: %w", multimodalChatNS, err)
	}

	if err := labelNamespaceDone(ctx, clientset, multimodalChatNS, expiresAt); err != nil {
		return fmt.Errorf("labelling namespace %s: %w", multimodalChatNS, err)
	}

	restartPods(ctx, clientset, multimodalChatNS, "app=multimodal-chatbot")

	log.Printf("[%s] MaaS credentials provisioned (openwebui + multimodal-chat)", tenantNS)
	return nil
}

// restartPods deletes pods matching a label selector in a namespace so they pick up secret changes.
func restartPods(ctx context.Context, clientset kubernetes.Interface, namespace, labelSelector string) {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		log.Printf("[%s] Warning: failed to list pods (%s): %v", namespace, labelSelector, err)
		return
	}
	for _, pod := range pods.Items {
		if err := clientset.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			log.Printf("[%s] Warning: failed to delete pod %s: %v", namespace, pod.Name, err)
		} else {
			log.Printf("[%s] Deleted pod %s for restart", namespace, pod.Name)
		}
	}
}

// upsertMultimodalChatbotSecret creates/updates the multimodal-chatbot secret.
// Uses CHATBOT_CONFIG as a template and fills in MaaS model endpoints and tokens.
// If CHATBOT_CONFIG is not set, creates the secret with an empty config.json.
func upsertMultimodalChatbotSecret(ctx context.Context, clientset kubernetes.Interface, tenantName, namespace, token string, models *maasModelsResponse, labels map[string]string) error {
	if chatbotConfigJSON == "" {
		log.Printf("[%s] CHATBOT_CONFIG not set, creating multimodal-chatbot with empty config.json", tenantName)
		return upsertSecret(ctx, clientset, namespace, "multimodal-chatbot", map[string][]byte{
			"config.json": []byte("{}"),
		}, labels)
	}

	type llmEntry struct {
		Name              string  `json:"name"`
		InferenceEndpoint string  `json:"inference_endpoint"`
		APIKey            string  `json:"api_key"`
		ModelName         string  `json:"model_name"`
		MaxTokens         int     `json:"max_tokens"`
		Temperature       float64 `json:"temperature"`
		TopP              float64 `json:"top_p"`
		PresencePenalty   float64 `json:"presence_penalty"`
		SupportsVision    bool    `json:"supports_vision"`
	}
	type chatbotConfig struct {
		SystemTemplate          string     `json:"system_template"`
		TranslateSystemTemplate string     `json:"translate_system_template"`
		LLMs                    []llmEntry `json:"llms"`
	}

	var cbConfig chatbotConfig
	if err := json.Unmarshal([]byte(chatbotConfigJSON), &cbConfig); err != nil {
		return fmt.Errorf("parsing CHATBOT_CONFIG: %w", err)
	}

	// Build MaaS model_name -> URL lookup
	maasModels := make(map[string]string)
	for _, m := range models.Data {
		trimmed := strings.TrimRight(m.URL, "/")
		if lastSlash := strings.LastIndex(trimmed, "/"); lastSlash >= 0 {
			modelName := trimmed[lastSlash+1:]
			maasModels[modelName] = m.URL + "/v1"
		}
	}

	// Fill in inference_endpoint and api_key for matching MaaS models
	updated := 0
	for i := range cbConfig.LLMs {
		if endpoint, exists := maasModels[cbConfig.LLMs[i].ModelName]; exists {
			cbConfig.LLMs[i].InferenceEndpoint = endpoint
			cbConfig.LLMs[i].APIKey = token
			updated++
			log.Printf("[%s] Matched MaaS model %q -> %s", tenantName, cbConfig.LLMs[i].ModelName, endpoint)
		}
	}
	log.Printf("[%s] multimodal-chatbot: %d/%d models matched from MaaS", tenantName, updated, len(cbConfig.LLMs))

	configJSON, err := json.MarshalIndent(cbConfig, "", "    ")
	if err != nil {
		return fmt.Errorf("marshalling config.json: %w", err)
	}

	return upsertSecret(ctx, clientset, namespace, "multimodal-chatbot", map[string][]byte{
		"config.json": configJSON,
	}, labels)
}

type maasModelsResponse struct {
	Data []struct {
		ID           string `json:"id"`
		URL          string `json:"url"`
		ModelDetails struct {
			DisplayName string `json:"displayName"`
		} `json:"modelDetails"`
	} `json:"data"`
}

func getMaaSTokenAndModels(ctx context.Context) (string, *maasModelsResponse, error) {
	maasHost := strings.TrimRight(maasURL, "/")

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 30 * time.Second,
	}

	tokenBody := strings.NewReader(fmt.Sprintf(`{"expiration": "%s"}`, tokenExpiry))
	req, err := http.NewRequestWithContext(ctx, "POST", maasHost+"/maas-api/v1/tokens", tokenBody)
	if err != nil {
		return "", nil, fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+maasToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("requesting MaaS token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("MaaS token request failed (status %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", nil, fmt.Errorf("decoding token response: %w", err)
	}
	if tokenResp.Token == "" {
		return "", nil, fmt.Errorf("empty token in MaaS response")
	}
	log.Printf("Obtained MaaS token (expiry: %s)", tokenExpiry)

	req, err = http.NewRequestWithContext(ctx, "GET", maasHost+"/maas-api/v1/models", nil)
	if err != nil {
		return "", nil, fmt.Errorf("creating models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenResp.Token)
	req.Header.Set("Content-Type", "application/json")

	modelsResp, err := httpClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("requesting MaaS models: %w", err)
	}
	defer modelsResp.Body.Close()

	if modelsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(modelsResp.Body)
		return "", nil, fmt.Errorf("MaaS models request failed (status %d): %s", modelsResp.StatusCode, string(body))
	}

	var models maasModelsResponse
	if err := json.NewDecoder(modelsResp.Body).Decode(&models); err != nil {
		return "", nil, fmt.Errorf("decoding models response: %w", err)
	}
	if len(models.Data) == 0 {
		return "", nil, fmt.Errorf("no models available in MaaS response")
	}

	return tokenResp.Token, &models, nil
}

func buildModelData(token string, models *maasModelsResponse) (string, string) {
	var modelURLs []string
	var modelTokens []string
	for _, m := range models.Data {
		modelURLs = append(modelURLs, m.URL+"/v1")
		modelTokens = append(modelTokens, token)
	}
	return strings.Join(modelURLs, ";"), strings.Join(modelTokens, ";")
}


// upsertConfigMap creates or updates a ConfigMap.
func upsertConfigMap(ctx context.Context, clientset kubernetes.Interface, namespace, name string, data map[string]string, labels map[string]string) error {
	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    labels,
			},
			Data: data,
		}
		if _, err := clientset.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return err
		}
		log.Printf("Created configmap %s in %s", name, namespace)
		return nil
	}
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	for k, v := range data {
		cm.Data[k] = v
	}
	if cm.Labels == nil {
		cm.Labels = make(map[string]string)
	}
	for k, v := range labels {
		cm.Labels[k] = v
	}
	if _, err := clientset.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return err
	}
	log.Printf("Updated configmap %s in %s", name, namespace)
	return nil
}

// upsertSecret creates or updates a Secret.
func upsertSecret(ctx context.Context, clientset kubernetes.Interface, namespace, name string, data map[string][]byte, labels map[string]string) error {
	secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    labels,
			},
			Data: data,
		}
		if _, err := clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return err
		}
		log.Printf("Created secret %s in %s", name, namespace)
		return nil
	}
	if err != nil {
		return err
	}
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	for k, v := range data {
		secret.Data[k] = v
	}
	if secret.Labels == nil {
		secret.Labels = make(map[string]string)
	}
	for k, v := range labels {
		secret.Labels[k] = v
	}
	if _, err := clientset.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return err
	}
	log.Printf("Updated secret %s in %s", name, namespace)
	return nil
}
