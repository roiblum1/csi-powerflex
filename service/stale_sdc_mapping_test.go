// Copyright © 2026 Dell Inc. or its subsidiaries. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//      http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package service

import (
	"errors"
	"strings"
	"testing"

	siotypes "github.com/dell/goscaleio/types/v1"
)

func testSdc(id, guid, ip, state string, extraIPs ...string) siotypes.Sdc {
	return siotypes.Sdc{ID: id, SdcGUID: guid, SdcIP: ip, SdcIPs: extraIPs, MdmConnectionState: state}
}

func testMapping(sdcID, ip, hostType string) *siotypes.MappedSdcInfo {
	return &siotypes.MappedSdcInfo{SdcID: sdcID, SdcIP: ip, HostType: hostType}
}

func staleIDs(in []staleSdcMapping) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, s.Stale.ID+"->"+s.Successor.ID)
	}
	return out
}

func TestFindSupersededSdcMappings(t *testing.T) {
	const (
		nodeA     = "aaaa000000000001" // current SDC of node A (re-imaged)
		nodeAOld  = "aaaa000000000000" // previous registration of node A
		nodeB     = "bbbb000000000002" // node B, healthy
		nodeCDown = "cccc000000000003" // node C, powered off
	)
	sdcs := []siotypes.Sdc{
		testSdc(nodeAOld, "GUID-A-OLD", "10.0.0.1", sdcStateDisconnected),
		testSdc(nodeA, "GUID-A-NEW", "10.0.0.1", sdcStateConnected),
		testSdc(nodeB, "GUID-B", "10.0.0.2", sdcStateConnected),
		testSdc(nodeCDown, "GUID-C", "10.0.0.3", sdcStateDisconnected),
	}

	tests := []struct {
		name        string
		mappings    []*siotypes.MappedSdcInfo
		sdcs        []siotypes.Sdc
		keep        string
		successorID string
		want        []string
	}{
		{
			name:     "old registration of re-imaged node is stale (publish to another node)",
			mappings: []*siotypes.MappedSdcInfo{testMapping(nodeAOld, "10.0.0.1", sdcHostType)},
			sdcs:     sdcs, keep: nodeB,
			want: []string{nodeAOld + "->" + nodeA},
		},
		{
			name:     "PowerFlex 3.6 empty HostType is treated as SDC",
			mappings: []*siotypes.MappedSdcInfo{testMapping(nodeAOld, "10.0.0.1", "")},
			sdcs:     sdcs, keep: nodeB,
			want: []string{nodeAOld + "->" + nodeA},
		},
		{
			name:     "unpublish from node A only cleans A's own old registration",
			mappings: []*siotypes.MappedSdcInfo{testMapping(nodeAOld, "10.0.0.1", sdcHostType)},
			sdcs:     sdcs, keep: nodeA, successorID: nodeA,
			want: []string{nodeAOld + "->" + nodeA},
		},
		{
			name:     "successor filter excludes other nodes' old registrations",
			mappings: []*siotypes.MappedSdcInfo{testMapping(nodeAOld, "10.0.0.1", sdcHostType)},
			sdcs:     sdcs, keep: nodeB, successorID: nodeB,
			want: nil,
		},
		{
			name:     "live SDC on another node is never stale",
			mappings: []*siotypes.MappedSdcInfo{testMapping(nodeB, "10.0.0.2", sdcHostType)},
			sdcs:     sdcs, keep: nodeA,
			want: nil,
		},
		{
			name:     "powered-off node (disconnected, no successor) is not stale",
			mappings: []*siotypes.MappedSdcInfo{testMapping(nodeCDown, "10.0.0.3", sdcHostType)},
			sdcs:     sdcs, keep: nodeB,
			want: nil,
		},
		{
			name:     "mapping to the requesting SDC is kept",
			mappings: []*siotypes.MappedSdcInfo{testMapping(nodeAOld, "10.0.0.1", sdcHostType)},
			sdcs:     sdcs, keep: nodeAOld,
			want: nil,
		},
		{
			name:     "NVMe host mapping is ignored",
			mappings: []*siotypes.MappedSdcInfo{testMapping(nodeAOld, "10.0.0.1", "NVMeHost")},
			sdcs:     sdcs, keep: nodeB,
			want: nil,
		},
		{
			name:     "SDC unknown to the system is not touched",
			mappings: []*siotypes.MappedSdcInfo{testMapping("ffff000000000009", "10.0.0.1", sdcHostType)},
			sdcs:     sdcs, keep: nodeB,
			want: nil,
		},
		{
			name:     "ambiguous address (two connected SDCs share the IP) is not stale",
			mappings: []*siotypes.MappedSdcInfo{testMapping(nodeAOld, "10.0.0.1", sdcHostType)},
			sdcs:     append(append([]siotypes.Sdc{}, sdcs...), testSdc("dddd000000000004", "GUID-D", "10.0.0.1", sdcStateConnected)),
			keep:     nodeB,
			want:     nil,
		},
		{
			name:     "connected SDC is never stale, even if another connected SDC shares its IP",
			mappings: []*siotypes.MappedSdcInfo{testMapping(nodeA, "10.0.0.1", sdcHostType)},
			sdcs:     append(append([]siotypes.Sdc{}, sdcs...), testSdc("dddd000000000004", "GUID-D", "10.0.0.1", sdcStateConnected)),
			keep:     nodeB,
			want:     nil,
		},
		{
			name:     "all data IPs (sdcIps) moved to one connected SDC",
			mappings: []*siotypes.MappedSdcInfo{testMapping("eeee000000000000", "192.168.10.5", sdcHostType)},
			sdcs: []siotypes.Sdc{
				testSdc("eeee000000000000", "GUID-E-OLD", "", sdcStateDisconnected, "192.168.10.5", "192.168.20.5"),
				testSdc("eeee000000000001", "GUID-E-NEW", "192.168.20.5", sdcStateConnected, "192.168.10.5"),
			},
			keep: nodeB,
			want: []string{"eeee000000000000->eeee000000000001"},
		},
		{
			name:     "only some data IPs moved is not stale",
			mappings: []*siotypes.MappedSdcInfo{testMapping("eeee000000000000", "192.168.10.5", sdcHostType)},
			sdcs: []siotypes.Sdc{
				testSdc("eeee000000000000", "GUID-E-OLD", "", sdcStateDisconnected, "192.168.10.5", "192.168.20.5"),
				testSdc("eeee000000000001", "GUID-E-NEW", "192.168.20.5", sdcStateConnected),
			},
			keep: nodeB,
			want: nil,
		},
		{
			name:     "data IPs split across two connected SDCs is not stale",
			mappings: []*siotypes.MappedSdcInfo{testMapping("eeee000000000000", "192.168.10.5", sdcHostType)},
			sdcs: []siotypes.Sdc{
				testSdc("eeee000000000000", "GUID-E-OLD", "", sdcStateDisconnected, "192.168.10.5", "192.168.20.5"),
				testSdc("eeee000000000001", "GUID-E-NEW", "192.168.20.5", sdcStateConnected),
				testSdc("ffff000000000001", "GUID-F", "192.168.10.5", sdcStateConnected),
			},
			keep: nodeB,
			want: nil,
		},
		{
			name: "duplicate mapping entries are reported once",
			mappings: []*siotypes.MappedSdcInfo{
				testMapping(nodeAOld, "10.0.0.1", sdcHostType),
				testMapping(nodeAOld, "10.0.0.1", sdcHostType),
			},
			sdcs: sdcs, keep: nodeB,
			want: []string{nodeAOld + "->" + nodeA},
		},
		{
			name:     "connection state compare is case-insensitive",
			mappings: []*siotypes.MappedSdcInfo{testMapping("x0", "10.9.9.9", sdcHostType)},
			sdcs: []siotypes.Sdc{
				testSdc("x0", "G0", "10.9.9.9", "disconnected"),
				testSdc("x1", "G1", "10.9.9.9", "CONNECTED"),
			},
			keep: nodeB,
			want: []string{"x0->x1"},
		},
		{
			name:     "nil mapping entries are skipped",
			mappings: []*siotypes.MappedSdcInfo{nil, testMapping(nodeAOld, "10.0.0.1", sdcHostType)},
			sdcs:     sdcs, keep: nodeB,
			want: []string{nodeAOld + "->" + nodeA},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := staleIDs(findSupersededSdcMappings(tc.mappings, tc.sdcs, tc.keep, tc.successorID))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRemainingMappings(t *testing.T) {
	mappings := []*siotypes.MappedSdcInfo{
		testMapping("old", "10.0.0.1", sdcHostType),
		testMapping("live", "10.0.0.2", sdcHostType),
		testMapping("old", "10.0.0.1", "NVMeHost"), // same ID but NVMe: must stay
		nil,
	}
	removed := []staleSdcMapping{{Stale: testSdc("old", "G", "10.0.0.1", sdcStateDisconnected)}}

	left := remainingMappings(mappings, removed)
	if len(left) != 2 || left[0].SdcID != "live" || left[1].HostType != "NVMeHost" {
		t.Fatalf("unexpected remaining mappings: %+v", left)
	}
	if got := remainingMappings(mappings[:1], removed); len(got) != 0 {
		t.Fatalf("expected no remaining mappings, got %+v", got)
	}
}

func TestDescribeMappingsShowsState(t *testing.T) {
	m := &siotypes.MappedSdcInfo{SdcID: "s1", SdcName: "worker-7", SdcIP: "10.0.0.7", HostType: sdcHostType}
	got := describeMappings([]*siotypes.MappedSdcInfo{m, nil}, []siotypes.Sdc{testSdc("s1", "GUID-7", "10.0.0.7", sdcStateDisconnected)})
	for _, want := range []string{"id=s1", `name="worker-7"`, "ip=10.0.0.7", "guid=GUID-7", "state=Disconnected"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q does not contain %q", got, want)
		}
	}
	if got := describeMappings([]*siotypes.MappedSdcInfo{m}, nil); !strings.Contains(got, "state=unknown") {
		t.Fatalf("expected unknown state, got %q", got)
	}
}

func TestHasForeignSdcMapping(t *testing.T) {
	vol := &siotypes.Volume{MappedSdcInfo: []*siotypes.MappedSdcInfo{testMapping("me", "", sdcHostType)}}
	if hasForeignSdcMapping(vol, "me") {
		t.Fatal("only own mapping: expected false")
	}
	vol.MappedSdcInfo = append(vol.MappedSdcInfo, testMapping("nvme", "", "NVMeHost"))
	if hasForeignSdcMapping(vol, "me") {
		t.Fatal("NVMe mapping must not count as foreign SDC mapping")
	}
	vol.MappedSdcInfo = append(vol.MappedSdcInfo, testMapping("other", "", ""))
	if !hasForeignSdcMapping(vol, "me") {
		t.Fatal("expected foreign mapping")
	}
	if hasForeignSdcMapping(nil, "me") {
		t.Fatal("nil volume: expected false")
	}
}

func TestSdcIPAddrs(t *testing.T) {
	s := testSdc("x", "g", " 10.0.0.1 ", "", "10.0.0.1", "", "10.0.0.2")
	got := strings.Join(sdcIPAddrs(&s), ",")
	if got != "10.0.0.1,10.0.0.2" {
		t.Fatalf("got %q", got)
	}
}

func TestIsNotMappedError(t *testing.T) {
	for _, err := range []error{
		&siotypes.Error{ErrorCode: sioErrVolumeNotMapped},
		&siotypes.Error{ErrorCode: sioErrVolumeNotMappedV4},
	} {
		if !isNotMappedError(err) {
			t.Fatalf("%v: expected true", err)
		}
	}
	for _, err := range []error{
		nil,
		errors.New("Volume not mapped to SDC"), // the text alone is not trusted
		&siotypes.Error{Message: "induced error", ErrorCode: 500},
	} {
		if isNotMappedError(err) {
			t.Fatalf("%v: expected false", err)
		}
	}
}
