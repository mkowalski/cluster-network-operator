package network

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"

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
//
// The CR carries the BGP sessions (neighbors, BFD) only. VIP advertisement
// deliberately does NOT use CRD prefixes/toAdvertise: frr-k8s renders
// router-level prefixes as unconditional `network` statements (bypassing the
// kube-vip health gate), and toAdvertise cannot express "advertise
// redistributed routes" at all - allowed mode=all only permits prefixes
// statically declared in router.prefixes and renders deny-any prefix-lists
// otherwise. Instead, advertisement happens exclusively via the rawConfig
// redistribution of routing table 198 (see buildBGPVIPRawConfig), into which
// kube-vip only installs routes for VIPs whose backends are healthy, and
// egress is opened by rawConfig permits appended to the per-neighbor
// <peer>-out route-maps that frr-k8s always renders.
func buildFRRConfigurationObjects(cfg bgpVIPConfigData) ([]*uns.Unstructured, error) {
	// Build neighbors list.
	neighbors := []interface{}{}
	for _, peer := range cfg.DefaultPeers {
		neighbor := map[string]interface{}{
			"address": peer.PeerAddress,
			"asn":     peer.PeerASN,
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
			"rawConfig": buildBGPVIPRawConfig(cfg),
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

// buildBGPVIPRawConfig renders the health-gated, leak-proof FRR raw config
// that advertises the VIPs, mirroring the semantics of MCO's bootstrap
// frr.conf.tmpl: redistribute routing table 198 (populated by kube-vip only
// for VIPs with healthy backends) filtered through route-maps that permit
// exactly the VIP prefixes and deny everything else, plus per-peer egress
// permits appended to the frr-k8s-generated <peer>-out route-maps.
// Address-family blocks, route-maps, prefix-lists, and egress permits are
// emitted only for families that have VIPs.
// A VIP containing ":" is IPv6 (/128), otherwise IPv4 (/32).
func buildBGPVIPRawConfig(cfg bgpVIPConfigData) string {
	var v4Prefixes, v6Prefixes []string
	for _, vip := range slices.Concat(cfg.APIVIPs, cfg.IngressVIPs) {
		if strings.Contains(vip, ":") {
			v6Prefixes = append(v6Prefixes, vip+"/128")
		} else {
			v4Prefixes = append(v4Prefixes, vip+"/32")
		}
	}

	var b strings.Builder
	// Zebra only tracks kernel routing tables other than main when
	// explicitly instructed. Without import-table zebra never sees the
	// kube-vip routes in table 198 and the table-direct redistribution
	// below exports nothing. The bootstrap config (MCO's
	// manifests/on-prem/frr.conf.tmpl) carries the same directive.
	b.WriteString("ip import-table 198\n")
	fmt.Fprintf(&b, "router bgp %d\n", cfg.LocalASN)
	if len(v4Prefixes) > 0 {
		b.WriteString(" address-family ipv4 unicast\n")
		b.WriteString("  redistribute table-direct 198 route-map BGP-VIP-ROUTES-V4\n")
		b.WriteString(" exit-address-family\n")
	}
	if len(v6Prefixes) > 0 {
		b.WriteString(" address-family ipv6 unicast\n")
		b.WriteString("  redistribute table-direct 198 route-map BGP-VIP-ROUTES-V6\n")
		b.WriteString(" exit-address-family\n")
	}
	if len(v4Prefixes) > 0 {
		b.WriteString("route-map BGP-VIP-ROUTES-V4 permit 10\n")
		b.WriteString(" match ip address prefix-list BGP-VIP-PREFIXES-V4\n")
		b.WriteString("route-map BGP-VIP-ROUTES-V4 deny 20\n")
	}
	if len(v6Prefixes) > 0 {
		b.WriteString("route-map BGP-VIP-ROUTES-V6 permit 10\n")
		b.WriteString(" match ipv6 address prefix-list BGP-VIP-PREFIXES-V6\n")
		b.WriteString("route-map BGP-VIP-ROUTES-V6 deny 20\n")
	}
	for i, prefix := range v4Prefixes {
		fmt.Fprintf(&b, "ip prefix-list BGP-VIP-PREFIXES-V4 seq %d permit %s\n", 10*(i+1), prefix)
	}
	for i, prefix := range v6Prefixes {
		fmt.Fprintf(&b, "ipv6 prefix-list BGP-VIP-PREFIXES-V6 seq %d permit %s\n", 10*(i+1), prefix)
	}
	// Open egress for exactly the VIP prefixes. frr-k8s always renders a
	// per-neighbor `route-map <peer>-out` (for non-VRF, non-interface peers
	// the neighbor ID naming the map is the peer address, see frr-k8s
	// internal/frr/config.go NeighborConfig.ID); with no toAdvertise its own
	// permit seqs match deny-any prefix-lists, because the CRD cannot express
	// "advertise redistributed routes" - allowed mode=all only covers
	// prefixes statically declared in router.prefixes (see frr-k8s
	// internal/controller/api_to_config.go prefixesToAdvertiseForFamily).
	// A route-map entry whose prefix-list denies is a no-match, which falls
	// through to the NEXT sequence rather than rejecting, so these high-seq
	// permits open egress ONLY for the VIP prefix-lists while everything
	// else stays implicitly denied - health-gated by the table-direct
	// redistribution above and leak-proof.
	for _, peer := range cfg.DefaultPeers {
		if len(v4Prefixes) > 0 {
			fmt.Fprintf(&b, "route-map %s-out permit 4000\n", peer.PeerAddress)
			b.WriteString(" match ip address prefix-list BGP-VIP-PREFIXES-V4\n")
		}
		if len(v6Prefixes) > 0 {
			fmt.Fprintf(&b, "route-map %s-out permit 4001\n", peer.PeerAddress)
			b.WriteString(" match ipv6 address prefix-list BGP-VIP-PREFIXES-V6\n")
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
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
