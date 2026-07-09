package network

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	uns "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestBuildFRRConfigurationObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	raw := `{"localASN":64512,"defaultPeers":[{"peerAddress":"192.168.111.1","peerASN":64513}],"apiVIPs":["192.168.111.5"],"ingressVIPs":["192.168.111.4"]}`
	var cfg bgpVIPConfigData
	g.Expect(json.Unmarshal([]byte(raw), &cfg)).To(Succeed())
	g.Expect(cfg.DefaultPeers).To(HaveLen(1))

	objs, err := buildFRRConfigurationObjects(cfg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(1))
	g.Expect(objs[0].GetName()).To(Equal("bgp-vip-master"))
	routers, found, err := uns.NestedSlice(objs[0].Object, "spec", "bgp", "routers")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(routers).To(HaveLen(1))
	neighbors, found, err := uns.NestedSlice(routers[0].(map[string]interface{}), "neighbors")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(neighbors).To(HaveLen(1))

	// VIP advertisement must NOT use CRD prefixes: frr-k8s renders those as
	// unconditional `network` statements, which bypass the kube-vip health
	// gate (routing table 198). No router-level prefixes, no prefixes list
	// under toAdvertise.
	_, found, err = uns.NestedStringSlice(routers[0].(map[string]interface{}), "prefixes")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeFalse())
	neighbor := neighbors[0].(map[string]interface{})
	// Egress must be opened explicitly: frr-k8s renders deny-all outbound
	// route-maps when toAdvertise is absent. mode=all is safe because the
	// filtered table-direct redistribution is the ingress filter, so only
	// the VIP prefixes exist in the BGP table to advertise.
	mode, found, err := uns.NestedString(neighbor, "toAdvertise", "allowed", "mode")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(mode).To(Equal("all"))
	_, found, err = uns.NestedStringSlice(neighbor, "toAdvertise", "allowed", "prefixes")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeFalse())
	g.Expect(neighbor["address"]).To(Equal("192.168.111.1"))
	g.Expect(neighbor["asn"]).To(Equal(int64(64513)))

	// Advertisement happens exclusively via health-gated redistribution of
	// routing table 198, filtered to exactly the VIP prefixes.
	rawConfig, found, err := uns.NestedString(objs[0].Object, "spec", "raw", "rawConfig")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	// Zebra only tracks non-main kernel tables when told to: without
	// import-table the table-direct redistribution exports nothing.
	g.Expect(rawConfig).To(HavePrefix("ip import-table 198\n"))
	g.Expect(rawConfig).To(ContainSubstring("router bgp 64512"))
	g.Expect(rawConfig).To(ContainSubstring("redistribute table-direct 198 route-map BGP-VIP-ROUTES-V4"))
	g.Expect(rawConfig).To(ContainSubstring("route-map BGP-VIP-ROUTES-V4 permit 10"))
	g.Expect(rawConfig).To(ContainSubstring("match ip address prefix-list BGP-VIP-PREFIXES-V4"))
	g.Expect(rawConfig).To(ContainSubstring("route-map BGP-VIP-ROUTES-V4 deny 20"))
	g.Expect(rawConfig).To(ContainSubstring("ip prefix-list BGP-VIP-PREFIXES-V4 seq 10 permit 192.168.111.5/32"))
	g.Expect(rawConfig).To(ContainSubstring("ip prefix-list BGP-VIP-PREFIXES-V4 seq 20 permit 192.168.111.4/32"))
	// No IPv6 VIPs: the v6 address-family, route-map, and prefix-list blocks
	// must be omitted entirely.
	g.Expect(rawConfig).NotTo(ContainSubstring("address-family ipv6"))
	g.Expect(rawConfig).NotTo(ContainSubstring("BGP-VIP-ROUTES-V6"))
	g.Expect(rawConfig).NotTo(ContainSubstring("BGP-VIP-PREFIXES-V6"))
}

// TestBuildFRRConfigurationObjectsAllOptionalFields locks the full
// installer-shaped config.json payload, with every optional field populated
// exactly as the installer emits them (string-typed bfdEnabled/ebgpMultiHop,
// see installer pkg/types/baremetal BGPPeerConfig).
func TestBuildFRRConfigurationObjectsAllOptionalFields(t *testing.T) {
	g := NewGomegaWithT(t)
	raw := `{"localASN":64512,"defaultPeers":[{"peerAddress":"192.168.111.1","peerASN":64513,"password":"s3cret","bfdEnabled":"true","ebgpMultiHop":"true","holdTime":"90s","keepaliveTime":"30s"}],"communities":["64512:100"],"apiVIPs":["192.168.111.5"],"ingressVIPs":["192.168.111.4"],"hostOverrides":{"master-0":[{"peerAddress":"192.168.1.1","peerASN":64513}]}}`
	var cfg bgpVIPConfigData
	g.Expect(json.Unmarshal([]byte(raw), &cfg)).To(Succeed())
	g.Expect(cfg.DefaultPeers).To(HaveLen(1))
	g.Expect(cfg.HostOverrides).To(HaveLen(1))
	g.Expect(cfg.HostOverrides["master-0"]).To(HaveLen(1))

	objs, err := buildFRRConfigurationObjects(cfg)
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
	// ebgpMultiHop must be emitted as a real bool in the CR even though the
	// installer serializes it as a string.
	g.Expect(neighbor["ebgpMultiHop"]).To(Equal(true))
	g.Expect(neighbor["password"]).To(Equal("s3cret"))
	g.Expect(neighbor["bfdProfile"]).To(Equal("vip-bfd"))
	// Sessions carry permit-all egress (frr-k8s renders deny-all outbound
	// route-maps when toAdvertise is absent), but advertisement content is
	// still controlled exclusively by gated redistribution in rawConfig:
	// no prefixes list under toAdvertise, no router-level prefixes.
	mode, found, err := uns.NestedString(neighbor, "toAdvertise", "allowed", "mode")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(mode).To(Equal("all"))
	_, found, err = uns.NestedStringSlice(neighbor, "toAdvertise", "allowed", "prefixes")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeFalse())
	_, found, err = uns.NestedStringSlice(routers[0].(map[string]interface{}), "prefixes")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeFalse())

	rawConfig, found, err := uns.NestedString(objs[0].Object, "spec", "raw", "rawConfig")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(rawConfig).To(HavePrefix("ip import-table 198\n"))
	g.Expect(rawConfig).To(ContainSubstring("redistribute table-direct 198 route-map BGP-VIP-ROUTES-V4"))
	g.Expect(rawConfig).To(ContainSubstring("route-map BGP-VIP-ROUTES-V4 permit 10"))
	g.Expect(rawConfig).To(ContainSubstring("route-map BGP-VIP-ROUTES-V4 deny 20"))
	g.Expect(rawConfig).To(ContainSubstring("ip prefix-list BGP-VIP-PREFIXES-V4 seq 10 permit 192.168.111.5/32"))
	g.Expect(rawConfig).To(ContainSubstring("ip prefix-list BGP-VIP-PREFIXES-V4 seq 20 permit 192.168.111.4/32"))

	bfdProfiles, found, err := uns.NestedSlice(objs[0].Object, "spec", "bgp", "bfdProfiles")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(bfdProfiles).To(HaveLen(1))
}

// TestBuildFRRConfigurationObjectsDualStack locks the general address-family
// handling: VIPs containing ":" are IPv6 (/128) and go to the V6 route-map
// and prefix-list; both families are emitted when both have VIPs.
func TestBuildFRRConfigurationObjectsDualStack(t *testing.T) {
	g := NewGomegaWithT(t)
	raw := `{"localASN":64512,"defaultPeers":[{"peerAddress":"192.168.111.1","peerASN":64513}],"apiVIPs":["192.168.111.5","fd2e:6f44:5dd8::5"],"ingressVIPs":["192.168.111.4","fd2e:6f44:5dd8::4"]}`
	var cfg bgpVIPConfigData
	g.Expect(json.Unmarshal([]byte(raw), &cfg)).To(Succeed())

	objs, err := buildFRRConfigurationObjects(cfg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(objs).To(HaveLen(1))

	rawConfig, found, err := uns.NestedString(objs[0].Object, "spec", "raw", "rawConfig")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(rawConfig).To(HavePrefix("ip import-table 198\n"))
	g.Expect(rawConfig).To(ContainSubstring("redistribute table-direct 198 route-map BGP-VIP-ROUTES-V4"))
	g.Expect(rawConfig).To(ContainSubstring("redistribute table-direct 198 route-map BGP-VIP-ROUTES-V6"))
	g.Expect(rawConfig).To(ContainSubstring("route-map BGP-VIP-ROUTES-V6 permit 10"))
	g.Expect(rawConfig).To(ContainSubstring("match ipv6 address prefix-list BGP-VIP-PREFIXES-V6"))
	g.Expect(rawConfig).To(ContainSubstring("route-map BGP-VIP-ROUTES-V6 deny 20"))
	g.Expect(rawConfig).To(ContainSubstring("ip prefix-list BGP-VIP-PREFIXES-V4 seq 10 permit 192.168.111.5/32"))
	g.Expect(rawConfig).To(ContainSubstring("ip prefix-list BGP-VIP-PREFIXES-V4 seq 20 permit 192.168.111.4/32"))
	g.Expect(rawConfig).To(ContainSubstring("ipv6 prefix-list BGP-VIP-PREFIXES-V6 seq 10 permit fd2e:6f44:5dd8::5/128"))
	g.Expect(rawConfig).To(ContainSubstring("ipv6 prefix-list BGP-VIP-PREFIXES-V6 seq 20 permit fd2e:6f44:5dd8::4/128"))
}
