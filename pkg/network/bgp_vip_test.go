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
}
