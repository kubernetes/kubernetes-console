package cache

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/Yiling-J/theine-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"k8s.io/dashboard/client/args"
	"k8s.io/dashboard/errors"
	"k8s.io/dashboard/helpers"
	"k8s.io/dashboard/types"
)

// contextCache is used when `cluster-context-enabled=true`. It maps
// a token to the context ID. It is used only when client needs to cache
// multi-cluster resources.
var contextCache *theine.Cache[string, string]

func init() {
	var err error
	if contextCache, err = theine.NewBuilder[string, string](int64(args.CacheSize())).Build(); err != nil {
		panic(err)
	}
}

// key is an internal structure used for creating
// a unique cache key SHA. It is used when
// `cluster-context-enabled=false`.
type key struct {
	// kind is a Kubernetes resource kind.
	kind types.ResourceKind

	// namespace is a Kubernetes resource namespace.
	namespace string

	// opts is a list options object used by the Kubernetes client.
	opts metav1.ListOptions
}

// SHA calculates sha based on the internal key fields.
func (k key) SHA() (string, error) {
	return helpers.HashObject(k)
}

func (k key) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Kind      types.ResourceKind
		Namespace string
		Opts      metav1.ListOptions
	}{
		Kind:      k.kind,
		Namespace: k.namespace,
		Opts:      metav1.ListOptions{LabelSelector: k.opts.LabelSelector, FieldSelector: k.opts.FieldSelector},
	})
}

// Key embeds an internal key structure and extends it with the support
// for the multi-cluster cache key creation. It is used when
// `cluster-context-enabled=true`.
type Key struct {
	key

	// token is an auth token used to exchange it for the context ID.
	token string

	// context is an internal identifier used in conjunction with the key
	// structure fields to create a cache key SHA that will be unique across
	// all clusters.
	context string
}

func (k Key) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		K       key
		Context string
	}{
		K:       k.key,
		Context: k.context,
	})
}

// SHA calculates sha based on the internal struct fields.
// It is also responsible for exchanging the token for
// the context identifier with the external source of truth
// configured via `token-exchange-endpoint` flag.
func (k Key) SHA() (sha string, err error) {
	if !args.ClusterContextEnabled() {
		return k.key.SHA()
	}

	contextKey, exists := contextCache.Get(k.token)
	if !exists {
		contextKey, err = k.exchangeToken(k.token)
		if err != nil {
			return "", err
		}

		contextCache.SetWithTTL(k.token, contextKey, 1, args.CacheTTL())
	}

	k.context = contextKey
	return helpers.HashObject(k)
}

func (k Key) exchangeToken(token string) (string, error) {
	client := &http.Client{Transport: &tokenExchangeTransport{token, http.DefaultTransport}}
	response, err := client.Get(args.TokenExchangeEndpoint())
	if err != nil {
		return "", err
	}

	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return "", errors.NewUnauthorized(fmt.Sprintf("could not exchange token: %s", response.Status))
	}

	if response.StatusCode != http.StatusOK {
		klog.ErrorS(errors.NewBadRequest(response.Status), "could not exchange token", "url", args.TokenExchangeEndpoint())
		return "", errors.NewBadRequest(response.Status)
	}

	defer func(body io.ReadCloser) {
		if err := body.Close(); err != nil {
			klog.ErrorS(err, "could not close response body writer")
		}
	}(response.Body)

	contextKey, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}

	klog.V(3).InfoS("token exchange successful", "context", contextKey)
	return string(contextKey), nil
}

// NewKey creates a new cache Key.
func NewKey(kind types.ResourceKind, namespace, token string, opts metav1.ListOptions) Key {
	return Key{key: key{kind, namespace, opts}, token: token}
}

type tokenExchangeTransport struct {
	token     string
	transport http.RoundTripper
}

func (in *tokenExchangeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+in.token)
	return in.transport.RoundTrip(req)
}