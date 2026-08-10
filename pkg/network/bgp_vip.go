package network

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	apifeatures "github.com/openshift/api/features"
	mcfgv1alpha1 "github.com/openshift/api/machineconfiguration/v1alpha1"
	operv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-network-operator/pkg/bootstrap"
	cnoclient "github.com/openshift/cluster-network-operator/pkg/client"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	uns "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
)

// apiCallTimeout bounds direct API calls made while rendering.
const apiCallTimeout = 30 * time.Second

// BGPVIPConfigGVR locates the BGPVIPConfig singleton read via the dynamic
// client; CNO has no typed clientset for machineconfiguration.openshift.io.
// Shared with the operconfig controller informer wiring.
var BGPVIPConfigGVR = schema.GroupVersionResource{
	Group:    "machineconfiguration.openshift.io",
	Version:  "v1alpha1",
	Resource: "bgpvipconfigs",
}

// validateBGPVIPInfraVIPs is the cross-object check the BGPVIPConfig CRD
// cannot express: the VIPs come from the Infrastructure CR and are spliced
// into the FRRConfiguration rawConfig, so they must be valid IPs. Spec-shape
// validation (ASN ranges, peer addresses, community format, enums,
// durations) is owned by the CRD's admission-time validation.
func validateBGPVIPInfraVIPs(apiVIPs, ingressVIPs []string) error {
	if len(apiVIPs)+len(ingressVIPs) == 0 {
		return fmt.Errorf("BGP-based VIP management requires API or Ingress VIPs on the Infrastructure CR")
	}
	for _, vip := range slices.Concat(apiVIPs, ingressVIPs) {
		if net.ParseIP(vip) == nil {
			return fmt.Errorf("VIP %q is not a valid IP address", vip)
		}
	}
	return nil
}

// isBGPVIPManagement reports whether the BGPBasedVIPManagement gate is
// enabled and the BareMetal Infrastructure CR requests vipManagement "BGP".
// The typed Infrastructure status from the bootstrap result carries the
// field now that openshift/api#2923 is vendored.
func isBGPVIPManagement(bootstrapResult *bootstrap.BootstrapResult, featureGates featuregates.FeatureGate) bool {
	if bootstrapResult == nil || bootstrapResult.Infra.PlatformStatus == nil ||
		bootstrapResult.Infra.PlatformType != configv1.BareMetalPlatformType ||
		bootstrapResult.Infra.PlatformStatus.BareMetal == nil {
		return false
	}
	// Enabled panics on unknown gates.
	if !slices.Contains(featureGates.KnownFeatures(), apifeatures.FeatureGateBGPBasedVIPManagement) ||
		!featureGates.Enabled(apifeatures.FeatureGateBGPBasedVIPManagement) {
		return false
	}
	return bootstrapResult.Infra.PlatformStatus.BareMetal.VIPManagement == configv1.VIPManagementTypeBGP
}

// renderBGPVIPFRRConfiguration builds FRRConfiguration CRs for BGP-managed
// VIPs from the BGPVIPConfig CR; a no-op unless BGP VIP management is
// active. It reports the outcome on the CR's SessionsConfigured condition.
func renderBGPVIPFRRConfiguration(conf *operv1.NetworkSpec, client cnoclient.Client, bootstrapResult *bootstrap.BootstrapResult, isBGP bool) ([]*uns.Unstructured, error) {
	if !isBGP {
		return nil, nil
	}
	// The FRRConfiguration CRD ships with the frr-k8s bundle. Checked
	// before the CR read: without the provider there is nothing to render
	// into (and no condition is written since the CR was not consulted).
	if conf == nil || conf.AdditionalRoutingCapabilities == nil ||
		!slices.Contains(conf.AdditionalRoutingCapabilities.Providers, operv1.RoutingCapabilitiesProviderFRR) {
		return nil, fmt.Errorf("BGP-based VIP management requires the FRR additional routing capability provider")
	}

	klog.Infof("BGP VIP management is active, rendering FRRConfiguration CRs")

	ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()
	obj, err := client.Default().Dynamic().Resource(BGPVIPConfigGVR).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("BGPVIPConfig \"cluster\" not found yet, skipping FRRConfiguration rendering")
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read BGPVIPConfig: %v", err)
	}
	bgpConfig := &mcfgv1alpha1.BGPVIPConfig{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, bgpConfig); err != nil {
		return nil, setBGPVIPSessionsCondition(client, bgpConfig,
			fmt.Errorf("failed to convert BGPVIPConfig: %v", err))
	}

	apiVIPs, ingressVIPs := bgpVIPInfraVIPs(bootstrapResult)
	if err := validateBGPVIPInfraVIPs(apiVIPs, ingressVIPs); err != nil {
		return nil, setBGPVIPSessionsCondition(client, bgpConfig, err)
	}

	if len(bgpConfig.Spec.HostOverrides) > 0 {
		// Overrides reach control plane nodes through the MCO-rendered
		// per-node peers file (runtimecfg keys them by short hostname);
		// the cluster-wide CR intentionally carries only defaultPeers.
		// See docs/bgp_vip_management.md.
		klog.Warningf("BGPVIPConfig hostOverrides apply to the static pod peers file only; the cluster-wide FRRConfiguration uses defaultPeers")
	}

	objs, err := buildFRRConfigurationObjects(bgpConfig.Spec, apiVIPs, ingressVIPs,
		func(name string) (*corev1.Secret, error) {
			ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
			defer cancel()
			return client.Default().Kubernetes().CoreV1().Secrets(bgpVIPSourceSecretNamespace).Get(ctx, name, metav1.GetOptions{})
		})
	if condErr := setBGPVIPSessionsCondition(client, bgpConfig, err); condErr != nil {
		return nil, condErr
	}
	return objs, nil
}

// bgpVIPInfraVIPs extracts the API and Ingress VIPs from the BareMetal
// Infrastructure status; isBGPVIPManagement guarantees it is present.
func bgpVIPInfraVIPs(bootstrapResult *bootstrap.BootstrapResult) ([]string, []string) {
	bm := bootstrapResult.Infra.PlatformStatus.BareMetal
	var apiVIPs, ingressVIPs []string
	for _, ip := range bm.APIServerInternalIPs {
		apiVIPs = append(apiVIPs, string(ip))
	}
	for _, ip := range bm.IngressIPs {
		ingressVIPs = append(ingressVIPs, string(ip))
	}
	return apiVIPs, ingressVIPs
}

// setBGPVIPSessionsCondition reports the render outcome on the BGPVIPConfig
// SessionsConfigured condition via server-side apply on the status
// subresource, owning only that condition type. It returns renderErr (or
// wraps it with the condition-write failure) so callers can pass their
// render error straight through.
func setBGPVIPSessionsCondition(client cnoclient.Client, bgpConfig *mcfgv1alpha1.BGPVIPConfig, renderErr error) error {
	condition := metav1.Condition{
		Type:               mcfgv1alpha1.BGPVIPConfigSessionsConfigured,
		Status:             metav1.ConditionTrue,
		Reason:             "AsExpected",
		Message:            "FRR session configuration rendered",
		ObservedGeneration: bgpConfig.Generation,
		LastTransitionTime: metav1.Now(),
	}
	if renderErr != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "RenderFailed"
		condition.Message = renderErr.Error()
	}
	// Preserve lastTransitionTime when the status did not change so SSA
	// resubmission does not churn the condition.
	if existing := meta.FindStatusCondition(bgpConfig.Status.Conditions, condition.Type); existing != nil &&
		existing.Status == condition.Status {
		condition.LastTransitionTime = existing.LastTransitionTime
	}

	patch := &uns.Unstructured{Object: map[string]interface{}{
		"apiVersion": mcfgv1alpha1.GroupVersion.String(),
		"kind":       "BGPVIPConfig",
		"metadata":   map[string]interface{}{"name": "cluster"},
		"status": map[string]interface{}{
			"conditions": []interface{}{map[string]interface{}{
				"type":               condition.Type,
				"status":             string(condition.Status),
				"reason":             condition.Reason,
				"message":            condition.Message,
				"observedGeneration": condition.ObservedGeneration,
				"lastTransitionTime": condition.LastTransitionTime.UTC().Format(time.RFC3339),
			}},
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()
	_, err := client.Default().Dynamic().Resource(BGPVIPConfigGVR).ApplyStatus(ctx, "cluster", patch,
		metav1.ApplyOptions{FieldManager: "cluster-network-operator", Force: true})
	if err != nil {
		if renderErr != nil {
			return fmt.Errorf("%v (additionally, failed to update BGPVIPConfig status: %v)", renderErr, err)
		}
		return fmt.Errorf("failed to update BGPVIPConfig status: %v", err)
	}
	return renderErr
}

// BGP VIP session password Secrets are referenced by name from
// openshift-config (the BGPVIPConfig API contract) but frr-k8s can only
// resolve passwordSecret references from its own namespace, so CNO copies
// them into openshift-frr-k8s.
const (
	bgpVIPSourceSecretNamespace = "openshift-config"
	bgpVIPTargetSecretNamespace = "openshift-frr-k8s"
)

// bgpVIPSecretGetter resolves a BGPVIPPeer passwordSecret name to the
// source Secret in openshift-config. Injected so tests stay client-free,
// like the pre-fetched CR spec.
type bgpVIPSecretGetter func(name string) (*corev1.Secret, error)

// buildFRRConfigurationObjects renders the cluster-wide (no node selector)
// FRRConfiguration. The CR spec carries only the BGP sessions; advertisement
// is in rawConfig because the CRD cannot express health-gated redistribution:
// declared prefixes render as unconditional `network` statements and
// toAdvertise only permits declared prefixes (metallb/frr-k8s#469).
// Per-node correctness comes from the health gate: kube-vip installs a
// table-198 route only where the VIP's backend is healthy.
func buildFRRConfigurationObjects(spec mcfgv1alpha1.BGPVIPConfigSpec, apiVIPs, ingressVIPs []string, getSecret bgpVIPSecretGetter) ([]*uns.Unstructured, error) {
	neighbors := []interface{}{}
	secretObjs := []*uns.Unstructured{}
	copiedSecrets := map[string]bool{}
	for _, peer := range spec.DefaultPeers {
		neighbor := map[string]interface{}{
			"address": peer.PeerAddress,
			"asn":     peer.PeerASN,
		}
		if name := peer.PasswordSecret.Name; name != "" {
			// frr-k8s resolves the reference from its own namespace, so
			// point it at the copy rendered below.
			neighbor["passwordSecret"] = map[string]interface{}{
				"name":      name,
				"namespace": bgpVIPTargetSecretNamespace,
			}
			if !copiedSecrets[name] {
				secretObj, err := buildBGPVIPSecretCopy(name, getSecret)
				if err != nil {
					return nil, err
				}
				secretObjs = append(secretObjs, secretObj)
				copiedSecrets[name] = true
			}
		}
		if peer.Port != 0 {
			neighbor["port"] = int64(peer.Port)
		}
		if peer.BFD == mcfgv1alpha1.BFDModeEnabled {
			neighbor["bfdProfile"] = "vip-bfd"
		}
		if peer.EBGPMultiHop == mcfgv1alpha1.EBGPMultiHopModeEnabled {
			neighbor["ebgpMultiHop"] = true
		}
		// Seconds render as the "<n>s" duration strings the ConfigMap
		// carried, keeping the FRRConfiguration byte-identical.
		if peer.HoldTimeSeconds != 0 {
			neighbor["holdTime"] = fmt.Sprintf("%ds", peer.HoldTimeSeconds)
		}
		if peer.KeepaliveTimeSeconds != 0 {
			neighbor["keepaliveTime"] = fmt.Sprintf("%ds", peer.KeepaliveTimeSeconds)
		}
		neighbors = append(neighbors, neighbor)
	}

	bfdProfiles := []interface{}{}
	for _, peer := range spec.DefaultPeers {
		if peer.BFD == mcfgv1alpha1.BFDModeEnabled {
			bfdProfiles = append(bfdProfiles, map[string]interface{}{
				"name":             "vip-bfd",
				"receiveInterval":  int64(300),
				"transmitInterval": int64(300),
			})
			break
		}
	}

	frrSpec := map[string]interface{}{
		"bgp": map[string]interface{}{
			"bfdProfiles": bfdProfiles,
			"routers": []interface{}{
				map[string]interface{}{
					"asn":       spec.LocalASN,
					"neighbors": neighbors,
				},
			},
		},
		"raw": map[string]interface{}{
			"rawConfig": buildBGPVIPRawConfig(spec, apiVIPs, ingressVIPs),
		},
	}

	obj := &uns.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "frrk8s.metallb.io/v1beta1",
			"kind":       "FRRConfiguration",
			"metadata": map[string]interface{}{
				"name":      "bgp-vip",
				"namespace": "openshift-frr-k8s",
				"labels": map[string]interface{}{
					"app.kubernetes.io/managed-by": "cluster-network-operator",
				},
			},
			"spec": frrSpec,
		},
	}

	return append([]*uns.Unstructured{obj}, secretObjs...), nil
}

// buildBGPVIPSecretCopy reads the referenced kubernetes.io/basic-auth
// Secret from openshift-config and renders an identical copy for
// openshift-frr-k8s, emitted through the same generic apply pipeline as
// the FRRConfiguration so ownership and pruning stay uniform.
func buildBGPVIPSecretCopy(name string, getSecret bgpVIPSecretGetter) (*uns.Unstructured, error) {
	source, err := getSecret(name)
	if err != nil {
		return nil, fmt.Errorf("failed to read BGP VIP password Secret %s/%s: %v", bgpVIPSourceSecretNamespace, name, err)
	}
	if source.Type != corev1.SecretTypeBasicAuth {
		return nil, fmt.Errorf("BGP VIP password Secret %s/%s has type %q, expected %q", bgpVIPSourceSecretNamespace, name, source.Type, corev1.SecretTypeBasicAuth)
	}
	copied := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: bgpVIPTargetSecretNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "cluster-network-operator",
			},
		},
		Type: source.Type,
		Data: source.Data,
	}
	out, err := runtime.DefaultUnstructuredConverter.ToUnstructured(copied)
	if err != nil {
		return nil, fmt.Errorf("failed to convert BGP VIP password Secret %s: %v", name, err)
	}
	return &uns.Unstructured{Object: out}, nil
}

// buildBGPVIPRawConfig mirrors MCO's bootstrap frr.conf.tmpl: redistribute
// kernel table 198 filtered to exactly the VIP prefixes, plus egress permits
// for them. Per-family blocks render only when that family has VIPs.
func buildBGPVIPRawConfig(spec mcfgv1alpha1.BGPVIPConfigSpec, apiVIPs, ingressVIPs []string) string {
	var v4Prefixes, v6Prefixes []string
	for _, vip := range slices.Concat(apiVIPs, ingressVIPs) {
		if strings.Contains(vip, ":") {
			v6Prefixes = append(v6Prefixes, vip+"/128")
		} else {
			v4Prefixes = append(v4Prefixes, vip+"/32")
		}
	}

	var b strings.Builder
	// table-direct reads the kernel table directly; no `ip import-table`.
	fmt.Fprintf(&b, "router bgp %d\n", spec.LocalASN)
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
	// Communities attach where the VIP routes enter the BGP table, so
	// every egress path carries them (parity with the static pod
	// template's VIP-COMMUNITY route-map).
	if len(v4Prefixes) > 0 {
		b.WriteString("route-map BGP-VIP-ROUTES-V4 permit 10\n")
		b.WriteString(" match ip address prefix-list BGP-VIP-PREFIXES-V4\n")
		for _, community := range spec.Communities {
			fmt.Fprintf(&b, " set community %s additive\n", community)
		}
		b.WriteString("route-map BGP-VIP-ROUTES-V4 deny 20\n")
	}
	if len(v6Prefixes) > 0 {
		b.WriteString("route-map BGP-VIP-ROUTES-V6 permit 10\n")
		b.WriteString(" match ipv6 address prefix-list BGP-VIP-PREFIXES-V6\n")
		for _, community := range spec.Communities {
			fmt.Fprintf(&b, " set community %s additive\n", community)
		}
		b.WriteString("route-map BGP-VIP-ROUTES-V6 deny 20\n")
	}
	for i, prefix := range v4Prefixes {
		fmt.Fprintf(&b, "ip prefix-list BGP-VIP-PREFIXES-V4 seq %d permit %s\n", 10*(i+1), prefix)
	}
	for i, prefix := range v6Prefixes {
		fmt.Fprintf(&b, "ipv6 prefix-list BGP-VIP-PREFIXES-V6 seq %d permit %s\n", 10*(i+1), prefix)
	}
	// Egress: frr-k8s renders per-neighbor `<address>-out` route-maps whose
	// generated entries deny everything without toAdvertise. A no-match
	// falls through, so these high-sequence permits open egress for exactly
	// the VIP prefixes. Couples to frr-k8s's route-map naming; native CRD
	// support is proposed in metallb/frr-k8s#469.
	for _, peer := range spec.DefaultPeers {
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
