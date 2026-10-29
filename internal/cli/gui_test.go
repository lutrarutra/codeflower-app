package cli

import "testing"

func TestGUIAddressDefaultsToAutomaticPortAndCanSelectOne(t *testing.T) {
	for _, test := range []struct {
		name        string
		address     string
		port        int
		addressSet  bool
		portSet     bool
		wantAddress string
	}{
		{name: "default automatic", address: "127.0.0.1:0", wantAddress: "127.0.0.1:0"},
		{name: "explicit automatic", address: "127.0.0.1:0", portSet: true, wantAddress: "127.0.0.1:0"},
		{name: "fixed port", address: "127.0.0.1:0", port: 8765, portSet: true, wantAddress: "127.0.0.1:8765"},
		{name: "full address", address: "127.0.0.1:8080", addressSet: true, wantAddress: "127.0.0.1:8080"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := guiAddress(test.address, test.port, test.addressSet, test.portSet)
			if err != nil || got != test.wantAddress {
				t.Fatalf("guiAddress() = %q, %v; want %q", got, err, test.wantAddress)
			}
		})
	}
}

func TestGUIAddressRejectsInvalidOrConflictingPortFlags(t *testing.T) {
	for _, test := range []struct {
		name       string
		address    string
		port       int
		addressSet bool
		portSet    bool
	}{
		{name: "negative port", address: "127.0.0.1:0", port: -1, portSet: true},
		{name: "too large port", address: "127.0.0.1:0", port: 65536, portSet: true},
		{name: "address and port", address: "127.0.0.1:8080", port: 8080, addressSet: true, portSet: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := guiAddress(test.address, test.port, test.addressSet, test.portSet); err == nil {
				t.Fatal("guiAddress() accepted invalid flags")
			}
		})
	}
}

func TestGUICommandPortDefaultsToAutomatic(t *testing.T) {
	flag := guiCommand().Flags().Lookup("port")
	if flag == nil || flag.DefValue != "0" {
		t.Fatalf("port flag default = %v; want 0 (automatic)", flag)
	}
}
