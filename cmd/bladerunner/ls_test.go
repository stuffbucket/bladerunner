package main

import (
	"testing"

	"github.com/lxc/incus/v6/shared/api"
)

func TestPrimaryIPv4(t *testing.T) {
	cases := []struct {
		name string
		inst api.InstanceFull
		want string
	}{
		{
			name: "nil state",
			inst: api.InstanceFull{},
			want: "",
		},
		{
			name: "skip loopback and link-local",
			inst: api.InstanceFull{
				State: &api.InstanceState{
					Network: map[string]api.InstanceStateNetwork{
						"lo": {
							Addresses: []api.InstanceStateNetworkAddress{
								{Family: addrFamilyIPv4,Address: "127.0.0.1", Scope: "local"},
							},
						},
						"eth0": {
							Addresses: []api.InstanceStateNetworkAddress{
								{Family: "inet6", Address: "fe80::1", Scope: "link"},
								{Family: addrFamilyIPv4,Address: "10.0.0.5", Scope: "global"},
							},
						},
					},
				},
			},
			want: "10.0.0.5",
		},
		{
			name: "no inet addresses",
			inst: api.InstanceFull{
				State: &api.InstanceState{
					Network: map[string]api.InstanceStateNetwork{
						"eth0": {
							Addresses: []api.InstanceStateNetworkAddress{
								{Family: "inet6", Address: "fd00::1", Scope: "global"},
							},
						},
					},
				},
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := primaryIPv4(&tc.inst)
			if got != tc.want {
				t.Fatalf("primaryIPv4: got %q want %q", got, tc.want)
			}
		})
	}
}

func TestImageSource(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]string
		want string
	}{
		{
			name: "image description preferred",
			cfg:  map[string]string{"image.description": "Ubuntu 24.04 LTS"},
			want: "Ubuntu 24.04 LTS",
		},
		{
			name: "fallback to os+release",
			cfg:  map[string]string{"image.os": "ubuntu", "image.release": "noble"},
			want: "ubuntu noble",
		},
		{
			name: "fallback to volatile base image fingerprint",
			cfg:  map[string]string{"volatile.base_image": "abcdef0123456789deadbeef"},
			want: "abcdef012345",
		},
		{
			name: "empty",
			cfg:  map[string]string{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := api.InstanceFull{}
			inst.Config = api.ConfigMap(tc.cfg)
			got := imageSource(&inst)
			if got != tc.want {
				t.Fatalf("imageSource: got %q want %q", got, tc.want)
			}
		})
	}
}
