package capability

import "testing"

func TestRegistrySortsAndClonesDescriptors(t *testing.T) {
	registry, err := New(
		Descriptor{Name: "proxy.socks5", Version: 1, Properties: map[string]string{"port": "1080"}},
		Descriptor{Name: "ip.observe", Version: 1},
	)
	if err != nil {
		t.Fatal(err)
	}

	listed := registry.List()
	if len(listed) != 2 || listed[0].Name != "ip.observe" || listed[1].Name != "proxy.socks5" {
		t.Fatalf("List() = %#v", listed)
	}
	listed[1].Properties["port"] = "1"
	if registry.List()[1].Properties["port"] != "1080" {
		t.Fatal("List() exposed mutable registry state")
	}
}

func TestRegistryRejectsDuplicateCapability(t *testing.T) {
	_, err := New(
		Descriptor{Name: "ip.observe", Version: 1},
		Descriptor{Name: "ip.observe", Version: 2},
	)
	if err == nil {
		t.Fatal("New() accepted duplicate capability")
	}
}
