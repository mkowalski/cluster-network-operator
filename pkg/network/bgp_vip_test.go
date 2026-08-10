package network

import (
	"encoding/base64"
	"reflect"
	"testing"

	. "github.com/onsi/gomega"
	mcfgv1alpha1 "github.com/openshift/api/machineconfiguration/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	uns "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestBuildFRRConfigurationObjectsTypedParity locks byte-identical rendering
// against the golden output the ConfigMap-based renderer produced for the
// all-optional-fields fixture (installer config.json shape). The typed CR
// enums/seconds must map to exactly the same FRRConfiguration fields:
// bfd "Enabled" -> bfdProfile "vip-bfd", ebgpMultiHop "Enabled" -> true,
// holdTimeSeconds 90 -> holdTime "90s", keepaliveTimeSeconds 30 -> "30s".
func TestBuildFRRConfigurationObjectsTypedParity(t *testing.T) {
	g := NewGomegaWithT(t)
	spec := mcfgv1alpha1.BGPVIPConfigSpec{
		LocalASN: 64512,
		DefaultPeers: []mcfgv1alpha1.BGPVIPPeer{{
			PeerAddress:          "192.168.111.1",
			PeerASN:              64513,
			BFD:                  mcfgv1alpha1.BFDModeEnabled,
			EBGPMultiHop:         mcfgv1alpha1.EBGPMultiHopModeEnabled,
			HoldTimeSeconds:      90,
			KeepaliveTimeSeconds: 30,
		}},
		Communities: []string{"64512:100"},
		HostOverrides: []mcfgv1alpha1.BGPVIPHostPeers{{
			Hostname: "master-0",
			Peers:    []mcfgv1alpha1.BGPVIPPeer{{PeerAddress: "192.168.1.1", PeerASN: 64513}},
		}},
	}

	objs, err := buildFRRConfigurationObjects(spec, []string{"192.168.111.5"}, []string{"192.168.111.4"}, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(1))

	// Golden output captured from the ConfigMap-based renderer at
	// bgp-vip-management (b272004d4) for the equivalent config.json fixture.
	expected := map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1",
		"kind":       "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name":      "bgp-vip",
			"namespace": "openshift-frr-k8s",
			"labels": map[string]interface{}{
				"app.kubernetes.io/managed-by": "cluster-network-operator",
			},
		},
		"spec": map[string]interface{}{
			"bgp": map[string]interface{}{
				"bfdProfiles": []interface{}{
					map[string]interface{}{
						"name":             "vip-bfd",
						"receiveInterval":  int64(300),
						"transmitInterval": int64(300),
					},
				},
				"routers": []interface{}{
					map[string]interface{}{
						"asn": int64(64512),
						"neighbors": []interface{}{
							map[string]interface{}{
								"address":       "192.168.111.1",
								"asn":           int64(64513),
								"bfdProfile":    "vip-bfd",
								"ebgpMultiHop":  true,
								"holdTime":      "90s",
								"keepaliveTime": "30s",
							},
						},
					},
				},
			},
			"raw": map[string]interface{}{
				"rawConfig": "router bgp 64512\n" +
					" address-family ipv4 unicast\n" +
					"  redistribute table-direct 198 route-map BGP-VIP-ROUTES-V4\n" +
					" exit-address-family\n" +
					"route-map BGP-VIP-ROUTES-V4 permit 10\n" +
					" match ip address prefix-list BGP-VIP-PREFIXES-V4\n" +
					" set community 64512:100 additive\n" +
					"route-map BGP-VIP-ROUTES-V4 deny 20\n" +
					"ip prefix-list BGP-VIP-PREFIXES-V4 seq 10 permit 192.168.111.5/32\n" +
					"ip prefix-list BGP-VIP-PREFIXES-V4 seq 20 permit 192.168.111.4/32\n" +
					"route-map 192.168.111.1-out permit 4000\n" +
					" match ip address prefix-list BGP-VIP-PREFIXES-V4",
			},
		},
	}
	if !reflect.DeepEqual(objs[0].Object, expected) {
		t.Errorf("typed rendering diverges from ConfigMap golden output:\ngot:  %#v\nwant: %#v", objs[0].Object, expected)
	}
}

func TestBuildFRRConfigurationObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	spec := mcfgv1alpha1.BGPVIPConfigSpec{
		LocalASN:     64512,
		DefaultPeers: []mcfgv1alpha1.BGPVIPPeer{{PeerAddress: "192.168.111.1", PeerASN: 64513}},
	}

	objs, err := buildFRRConfigurationObjects(spec, []string{"192.168.111.5"}, []string{"192.168.111.4"}, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(1))
	g.Expect(objs[0].GetName()).To(Equal("bgp-vip"))
	// No node selector: the CR applies to all nodes.
	_, found, err := uns.NestedMap(objs[0].Object, "spec", "nodeSelector")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeFalse())
	routers, found, err := uns.NestedSlice(objs[0].Object, "spec", "bgp", "routers")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(routers).To(HaveLen(1))
	neighbors, found, err := uns.NestedSlice(routers[0].(map[string]interface{}), "neighbors")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(neighbors).To(HaveLen(1))

	// CRD prefixes would render as unconditional `network` statements,
	// bypassing the health gate.
	_, found, err = uns.NestedStringSlice(routers[0].(map[string]interface{}), "prefixes")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeFalse())
	neighbor := neighbors[0].(map[string]interface{})
	_, found, err = uns.NestedMap(neighbor, "toAdvertise")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeFalse())
	g.Expect(neighbor["address"]).To(Equal("192.168.111.1"))
	g.Expect(neighbor["asn"]).To(Equal(int64(64513)))

	rawConfig, found, err := uns.NestedString(objs[0].Object, "spec", "raw", "rawConfig")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(rawConfig).To(HavePrefix("router bgp"))
	g.Expect(rawConfig).NotTo(ContainSubstring("import-table"))
	g.Expect(rawConfig).To(ContainSubstring("router bgp 64512"))
	g.Expect(rawConfig).To(ContainSubstring("redistribute table-direct 198 route-map BGP-VIP-ROUTES-V4"))
	g.Expect(rawConfig).To(ContainSubstring("route-map BGP-VIP-ROUTES-V4 permit 10"))
	g.Expect(rawConfig).To(ContainSubstring("match ip address prefix-list BGP-VIP-PREFIXES-V4"))
	g.Expect(rawConfig).To(ContainSubstring("route-map BGP-VIP-ROUTES-V4 deny 20"))
	g.Expect(rawConfig).To(ContainSubstring("ip prefix-list BGP-VIP-PREFIXES-V4 seq 10 permit 192.168.111.5/32"))
	g.Expect(rawConfig).To(ContainSubstring("ip prefix-list BGP-VIP-PREFIXES-V4 seq 20 permit 192.168.111.4/32"))
	g.Expect(rawConfig).To(ContainSubstring("route-map 192.168.111.1-out permit 4000\n match ip address prefix-list BGP-VIP-PREFIXES-V4"))
	// No IPv6 VIPs: no v6 blocks at all.
	g.Expect(rawConfig).NotTo(ContainSubstring("-out permit 4001"))
	g.Expect(rawConfig).NotTo(ContainSubstring("address-family ipv6"))
	g.Expect(rawConfig).NotTo(ContainSubstring("BGP-VIP-ROUTES-V6"))
	g.Expect(rawConfig).NotTo(ContainSubstring("BGP-VIP-PREFIXES-V6"))
}

// TestBuildFRRConfigurationObjectsAllOptionalFields locks the typed CR
// payload with every optional field set.
func TestBuildFRRConfigurationObjectsAllOptionalFields(t *testing.T) {
	g := NewGomegaWithT(t)
	spec := mcfgv1alpha1.BGPVIPConfigSpec{
		LocalASN: 64512,
		DefaultPeers: []mcfgv1alpha1.BGPVIPPeer{{
			PeerAddress:          "192.168.111.1",
			PeerASN:              64513,
			BFD:                  mcfgv1alpha1.BFDModeEnabled,
			EBGPMultiHop:         mcfgv1alpha1.EBGPMultiHopModeEnabled,
			HoldTimeSeconds:      90,
			KeepaliveTimeSeconds: 30,
		}},
		Communities: []string{"64512:100"},
		HostOverrides: []mcfgv1alpha1.BGPVIPHostPeers{{
			Hostname: "master-0",
			Peers:    []mcfgv1alpha1.BGPVIPPeer{{PeerAddress: "192.168.1.1", PeerASN: 64513}},
		}},
	}

	objs, err := buildFRRConfigurationObjects(spec, []string{"192.168.111.5"}, []string{"192.168.111.4"}, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(1))

	routers, found, err := uns.NestedSlice(objs[0].Object, "spec", "bgp", "routers")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(routers).To(HaveLen(1))
	neighbors, found, err := uns.NestedSlice(routers[0].(map[string]interface{}), "neighbors")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(neighbors).To(HaveLen(1))

	neighbor := neighbors[0].(map[string]interface{})
	// The typed enum maps to the bool the CR wants.
	g.Expect(neighbor["ebgpMultiHop"]).To(Equal(true))
	g.Expect(neighbor["holdTime"]).To(Equal("90s"))
	g.Expect(neighbor["keepaliveTime"]).To(Equal("30s"))
	g.Expect(neighbor["bfdProfile"]).To(Equal("vip-bfd"))
	_, found, err = uns.NestedMap(neighbor, "toAdvertise")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeFalse())
	_, found, err = uns.NestedStringSlice(routers[0].(map[string]interface{}), "prefixes")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeFalse())

	rawConfig, found, err := uns.NestedString(objs[0].Object, "spec", "raw", "rawConfig")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(rawConfig).To(HavePrefix("router bgp"))
	g.Expect(rawConfig).NotTo(ContainSubstring("import-table"))
	g.Expect(rawConfig).To(ContainSubstring("redistribute table-direct 198 route-map BGP-VIP-ROUTES-V4"))
	g.Expect(rawConfig).To(ContainSubstring("route-map BGP-VIP-ROUTES-V4 permit 10"))
	g.Expect(rawConfig).To(ContainSubstring(" set community 64512:100 additive"))
	g.Expect(rawConfig).To(ContainSubstring("route-map BGP-VIP-ROUTES-V4 deny 20"))
	g.Expect(rawConfig).To(ContainSubstring("ip prefix-list BGP-VIP-PREFIXES-V4 seq 10 permit 192.168.111.5/32"))
	g.Expect(rawConfig).To(ContainSubstring("ip prefix-list BGP-VIP-PREFIXES-V4 seq 20 permit 192.168.111.4/32"))
	g.Expect(rawConfig).To(ContainSubstring("route-map 192.168.111.1-out permit 4000\n match ip address prefix-list BGP-VIP-PREFIXES-V4"))

	bfdProfiles, found, err := uns.NestedSlice(objs[0].Object, "spec", "bgp", "bfdProfiles")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(bfdProfiles).To(HaveLen(1))
}

// TestBuildFRRConfigurationObjectsDualStack locks the per-family rendering.
func TestBuildFRRConfigurationObjectsDualStack(t *testing.T) {
	g := NewGomegaWithT(t)
	spec := mcfgv1alpha1.BGPVIPConfigSpec{
		LocalASN:     64512,
		DefaultPeers: []mcfgv1alpha1.BGPVIPPeer{{PeerAddress: "192.168.111.1", PeerASN: 64513}},
	}

	objs, err := buildFRRConfigurationObjects(spec,
		[]string{"192.168.111.5", "fd2e:6f44:5dd8::5"}, []string{"192.168.111.4", "fd2e:6f44:5dd8::4"}, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(1))

	rawConfig, found, err := uns.NestedString(objs[0].Object, "spec", "raw", "rawConfig")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(rawConfig).To(HavePrefix("router bgp"))
	g.Expect(rawConfig).NotTo(ContainSubstring("import-table"))
	g.Expect(rawConfig).To(ContainSubstring("redistribute table-direct 198 route-map BGP-VIP-ROUTES-V4"))
	g.Expect(rawConfig).To(ContainSubstring("redistribute table-direct 198 route-map BGP-VIP-ROUTES-V6"))
	g.Expect(rawConfig).To(ContainSubstring("route-map BGP-VIP-ROUTES-V6 permit 10"))
	g.Expect(rawConfig).To(ContainSubstring("match ipv6 address prefix-list BGP-VIP-PREFIXES-V6"))
	g.Expect(rawConfig).To(ContainSubstring("route-map BGP-VIP-ROUTES-V6 deny 20"))
	g.Expect(rawConfig).To(ContainSubstring("ip prefix-list BGP-VIP-PREFIXES-V4 seq 10 permit 192.168.111.5/32"))
	g.Expect(rawConfig).To(ContainSubstring("ip prefix-list BGP-VIP-PREFIXES-V4 seq 20 permit 192.168.111.4/32"))
	g.Expect(rawConfig).To(ContainSubstring("ipv6 prefix-list BGP-VIP-PREFIXES-V6 seq 10 permit fd2e:6f44:5dd8::5/128"))
	g.Expect(rawConfig).To(ContainSubstring("ipv6 prefix-list BGP-VIP-PREFIXES-V6 seq 20 permit fd2e:6f44:5dd8::4/128"))
	g.Expect(rawConfig).To(ContainSubstring("route-map 192.168.111.1-out permit 4000\n match ip address prefix-list BGP-VIP-PREFIXES-V4"))
	g.Expect(rawConfig).To(ContainSubstring("route-map 192.168.111.1-out permit 4001\n match ipv6 address prefix-list BGP-VIP-PREFIXES-V6"))
}

// TestValidateBGPVIPInfraVIPs covers the cross-object check the CRD cannot
// express: the VIPs come from the Infrastructure CR and must be valid IPs
// before they are spliced into the FRRConfiguration rawConfig.
func TestValidateBGPVIPInfraVIPs(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(validateBGPVIPInfraVIPs([]string{"192.168.111.5"}, []string{"fd2e:6f44:5dd8::4"})).To(Succeed())
	g.Expect(validateBGPVIPInfraVIPs([]string{"192.168.111.5\n router bgp 1"}, nil)).NotTo(Succeed())
	g.Expect(validateBGPVIPInfraVIPs(nil, []string{"not-an-ip"})).NotTo(Succeed())
	// No VIPs at all: BGP VIP management has nothing to advertise.
	g.Expect(validateBGPVIPInfraVIPs(nil, nil)).NotTo(Succeed())
}

// TestBuildFRRConfigurationObjectsPasswordSecret locks the passwordSecret
// mapping: the neighbor references the copy in openshift-frr-k8s (never an
// inline password), and the object list carries the copied
// kubernetes.io/basic-auth Secret rendered from openshift-config.
func TestBuildFRRConfigurationObjectsPasswordSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	spec := mcfgv1alpha1.BGPVIPConfigSpec{
		LocalASN: 64512,
		DefaultPeers: []mcfgv1alpha1.BGPVIPPeer{{
			PeerAddress:    "192.168.111.1",
			PeerASN:        64513,
			PasswordSecret: mcfgv1alpha1.BGPVIPPasswordSecret{Name: "bgp-peer-password"},
		}},
	}
	getSecret := func(name string) (*corev1.Secret, error) {
		g.Expect(name).To(Equal("bgp-peer-password"))
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "openshift-config"},
			Type:       corev1.SecretTypeBasicAuth,
			Data:       map[string][]byte{"password": []byte("s3cret")},
		}, nil
	}

	objs, err := buildFRRConfigurationObjects(spec, []string{"192.168.111.5"}, nil, getSecret)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(2))

	routers, _, err := uns.NestedSlice(objs[0].Object, "spec", "bgp", "routers")
	g.Expect(err).NotTo(HaveOccurred())
	neighbors, _, err := uns.NestedSlice(routers[0].(map[string]interface{}), "neighbors")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(neighbors).To(HaveLen(1))
	neighbor := neighbors[0].(map[string]interface{})
	// The reference points at the copy in the frr-k8s namespace; no
	// inline password is ever rendered.
	g.Expect(neighbor["passwordSecret"]).To(Equal(map[string]interface{}{
		"name":      "bgp-peer-password",
		"namespace": "openshift-frr-k8s",
	}))
	g.Expect(neighbor).NotTo(HaveKey("password"))

	secretObj := objs[1]
	g.Expect(secretObj.GetKind()).To(Equal("Secret"))
	g.Expect(secretObj.GetAPIVersion()).To(Equal("v1"))
	g.Expect(secretObj.GetName()).To(Equal("bgp-peer-password"))
	g.Expect(secretObj.GetNamespace()).To(Equal("openshift-frr-k8s"))
	secretType, _, err := uns.NestedString(secretObj.Object, "type")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(secretType).To(Equal(string(corev1.SecretTypeBasicAuth)))
	data, _, err := uns.NestedStringMap(secretObj.Object, "data")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(data).To(Equal(map[string]string{"password": base64.StdEncoding.EncodeToString([]byte("s3cret"))}))
}

// TestBuildFRRConfigurationObjectsPasswordSecretMissing locks the error
// path: a missing source Secret fails the render (Degraded +
// SessionsConfigured=False) instead of configuring unauthenticated
// sessions.
func TestBuildFRRConfigurationObjectsPasswordSecretMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	spec := mcfgv1alpha1.BGPVIPConfigSpec{
		LocalASN: 64512,
		DefaultPeers: []mcfgv1alpha1.BGPVIPPeer{{
			PeerAddress:    "192.168.111.1",
			PeerASN:        64513,
			PasswordSecret: mcfgv1alpha1.BGPVIPPasswordSecret{Name: "no-such-secret"},
		}},
	}
	getSecret := func(name string) (*corev1.Secret, error) {
		return nil, apierrors.NewNotFound(corev1.Resource("secrets"), name)
	}

	_, err := buildFRRConfigurationObjects(spec, []string{"192.168.111.5"}, nil, getSecret)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("openshift-config/no-such-secret"))
}
