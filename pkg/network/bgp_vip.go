package network

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"

	configv1 "github.com/openshift/api/config/v1"
	apifeatures "github.com/openshift/api/features"
	"github.com/openshift/cluster-network-operator/pkg/bootstrap"
	cnoclient "github.com/openshift/cluster-network-operator/pkg/client"
	"github.com/openshift/cluster-network-operator/pkg/names"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	uns "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// bgpVIPPeer represents a BGP peer configuration from the bgp-vip-config ConfigMap.
// String-typed BFDEnabled and EBGPMultiHop ("true"/"false") match the
// installer's BGPPeerConfig serialization.
type bgpVIPPeer struct {
	PeerAddress   string `json:"peerAddress"`
	PeerASN       int64  `json:"peerASN"`
	Password      string `json:"password,omitempty"`
	BFDEnabled    string `json:"bfdEnabled,omitempty"`
	EBGPMultiHop  string `json:"ebgpMultiHop,omitempty"`
	HoldTime      string `json:"holdTime,omitempty"`
	KeepaliveTime string `json:"keepaliveTime,omitempty"`
}

// bgpVIPConfigData is the parsed content of the bgp-vip-config ConfigMap's
// config.json key. The schema matches baremetal-runtimecfg's FRRPeerMapping.
type bgpVIPConfigData struct {
	LocalASN      int64                   `json:"localASN"`
	DefaultPeers  []bgpVIPPeer            `json:"defaultPeers"`
	Communities   []string                `json:"communities,omitempty"`
	APIVIPs       []string                `json:"apiVIPs"`
	IngressVIPs   []string                `json:"ingressVIPs"`
	HostOverrides map[string][]bgpVIPPeer `json:"hostOverrides,omitempty"`
}

// renderBGPVIPFRRConfiguration builds FRRConfiguration CRs for BGP-managed VIPs.
// It is a no-op when the BGPBasedVIPManagement feature gate is disabled, the
// platform is not BareMetal, or VIPManagement is not "BGP".
func renderBGPVIPFRRConfiguration(client cnoclient.Client, bootstrapResult *bootstrap.BootstrapResult, featureGates featuregates.FeatureGate) ([]*uns.Unstructured, error) {
	if bootstrapResult == nil || bootstrapResult.Infra.PlatformStatus == nil {
		return nil, nil
	}
	if bootstrapResult.Infra.PlatformType != configv1.BareMetalPlatformType {
		return nil, nil
	}
	if bootstrapResult.Infra.PlatformStatus.BareMetal == nil {
		return nil, nil
	}

	// Enabled panics on gates unknown to the cluster's FeatureGate status,
	// so guard the lookup (same idiom as the NoOverlayMode gate).
	if !slices.Contains(featureGates.KnownFeatures(), apifeatures.FeatureGateBGPBasedVIPManagement) ||
		!featureGates.Enabled(apifeatures.FeatureGateBGPBasedVIPManagement) {
		return nil, nil
	}
	if bootstrapResult.Infra.PlatformStatus.BareMetal.VIPManagement != "BGP" {
		return nil, nil
	}

	log.Printf("BGP VIP management is active, rendering FRRConfiguration CRs")

	// Read the bgp-vip-config ConfigMap.
	cm := &corev1.ConfigMap{}
	err := client.Default().CRClient().Get(context.TODO(),
		types.NamespacedName{Name: "bgp-vip-config", Namespace: names.APPLIED_NAMESPACE}, cm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// ConfigMap not yet available (may happen during early bootstrap).
			log.Printf("bgp-vip-config ConfigMap not found yet, skipping FRRConfiguration rendering")
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read bgp-vip-config ConfigMap: %v", err)
	}

	configJSON := cm.Data["config.json"]
	if configJSON == "" {
		return nil, fmt.Errorf("bgp-vip-config ConfigMap has no config.json data")
	}

	var bgpConfig bgpVIPConfigData
	if err := json.Unmarshal([]byte(configJSON), &bgpConfig); err != nil {
		return nil, fmt.Errorf("failed to parse bgp-vip-config: %v", err)
	}

	return buildFRRConfigurationObjects(bgpConfig)
}

// buildFRRConfigurationObjects constructs FRRConfiguration unstructured objects
// from the parsed BGP VIP config data.
func buildFRRConfigurationObjects(cfg bgpVIPConfigData) ([]*uns.Unstructured, error) {
	// Build VIP prefix list for toAdvertise.
	prefixes := []interface{}{}
	for _, vip := range cfg.APIVIPs {
		prefixes = append(prefixes, vip+"/32")
	}
	for _, vip := range cfg.IngressVIPs {
		prefixes = append(prefixes, vip+"/32")
	}

	// Build neighbors list.
	neighbors := []interface{}{}
	for _, peer := range cfg.DefaultPeers {
		neighbor := map[string]interface{}{
			"address": peer.PeerAddress,
			"asn":     peer.PeerASN,
			"toAdvertise": map[string]interface{}{
				"allowed": map[string]interface{}{
					"mode":     "filtered",
					"prefixes": prefixes,
				},
			},
		}
		if peer.Password != "" {
			neighbor["password"] = peer.Password
		}
		if peer.BFDEnabled == "true" {
			neighbor["bfdProfile"] = "vip-bfd"
		}
		if peer.EBGPMultiHop == "true" {
			neighbor["ebgpMultiHop"] = true
		}
		neighbors = append(neighbors, neighbor)
	}

	// Build BFD profiles if any peer uses BFD.
	bfdProfiles := []interface{}{}
	for _, peer := range cfg.DefaultPeers {
		if peer.BFDEnabled == "true" {
			bfdProfiles = append(bfdProfiles, map[string]interface{}{
				"name":             "vip-bfd",
				"receiveInterval":  int64(300),
				"transmitInterval": int64(300),
			})
			break
		}
	}

	// Build raw config for redistribute table-direct 198.
	rawConfig := fmt.Sprintf(`router bgp %d
 address-family ipv4 unicast
  redistribute table-direct 198
 exit-address-family
 address-family ipv6 unicast
  redistribute table-direct 198
 exit-address-family`, cfg.LocalASN)

	spec := map[string]interface{}{
		"bgp": map[string]interface{}{
			"bfdProfiles": bfdProfiles,
			"routers": []interface{}{
				map[string]interface{}{
					"asn":       cfg.LocalASN,
					"neighbors": neighbors,
				},
			},
		},
		"raw": map[string]interface{}{
			"rawConfig": rawConfig,
		},
		"nodeSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{
				"node-role.kubernetes.io/master": "",
			},
		},
	}

	obj := &uns.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "frrk8s.metallb.io/v1beta1",
			"kind":       "FRRConfiguration",
			"metadata": map[string]interface{}{
				"name":      "bgp-vip-master",
				"namespace": "openshift-frr-k8s",
				"labels": map[string]interface{}{
					"app.kubernetes.io/managed-by": "cluster-network-operator",
				},
			},
			"spec": spec,
		},
	}

	return []*uns.Unstructured{obj}, nil
}

// checkBGPSessionsEstablished checks whether all BGPSessionState resources
// report an "Established" status. This can be called from the reconciler
// after rendering to set degraded conditions.
func checkBGPSessionsEstablished(client cnoclient.Client) (bool, error) {
	sessionList := &uns.UnstructuredList{}
	sessionList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "frrk8s.metallb.io",
		Version: "v1beta1",
		Kind:    "BGPSessionStateList",
	})

	if err := client.Default().CRClient().List(context.TODO(), sessionList); err != nil {
		return false, err
	}

	for _, session := range sessionList.Items {
		status, _, _ := uns.NestedString(session.Object, "status", "bgpStatus")
		if status != "Established" {
			return false, nil
		}
	}
	return len(sessionList.Items) > 0, nil
}
