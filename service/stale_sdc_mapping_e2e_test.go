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

// End-to-end tests for the superseded-SDC handling in stale_sdc_mapping.go.
// They drive the real ControllerPublishVolume / ControllerUnpublishVolume
// against a fake PowerFlex REST endpoint that keeps volume-mapping state and
// records every map/unmap call, so they check what actually reaches the array.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	sio "github.com/dell/goscaleio"
	siotypes "github.com/dell/goscaleio/types/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	e2eSys    = "1a2b3c4d5e6f7081"
	e2eVolID  = "c0ffee0000000001"
	e2eCsiVol = e2eSys + "-" + e2eVolID
)

var (
	// worker-a was re-imaged: old registration Disconnected, new one Connected, same IP.
	e2eAOld = siotypes.Sdc{ID: "a000000000000000", SdcGUID: "AAAAAAAA-0000-0000-0000-000000000000", Name: "worker-a-old", SdcIP: "10.0.0.1", MdmConnectionState: "Disconnected"}
	e2eANew = siotypes.Sdc{ID: "a000000000000001", SdcGUID: "AAAAAAAA-1111-1111-1111-111111111111", Name: "worker-a", SdcIP: "10.0.0.1", MdmConnectionState: "Connected"}
	// worker-b healthy, worker-c powered off (Disconnected, nobody else on its IP).
	e2eB     = siotypes.Sdc{ID: "b000000000000002", SdcGUID: "BBBBBBBB-2222-2222-2222-222222222222", Name: "worker-b", SdcIP: "10.0.0.2", MdmConnectionState: "Connected"}
	e2eCDown = siotypes.Sdc{ID: "c000000000000003", SdcGUID: "CCCCCCCC-3333-3333-3333-333333333333", Name: "worker-c", SdcIP: "10.0.0.3", MdmConnectionState: "Disconnected"}
)

func e2eMap(s siotypes.Sdc) *siotypes.MappedSdcInfo {
	return &siotypes.MappedSdcInfo{SdcID: s.ID, SdcIP: s.SdcIP, SdcName: s.Name, HostType: "SdcHost"}
}

type e2eFailure struct {
	code int
	msg  string
}

type e2eArray struct {
	mu             sync.Mutex
	sdcs           []siotypes.Sdc
	mappings       []*siotypes.MappedSdcInfo // real state on the "array"
	getVolMappings []*siotypes.MappedSdcInfo // if set, GET volume returns this (stale view)
	failGetSdcFrom int                       // 1-based GetSdc call index from which it fails; 0 = never
	unmapFail      map[string]e2eFailure
	getSdcCalls    int
	actions        []string
}

func (f *e2eArray) writeErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(siotypes.Error{Message: msg, HTTPStatusCode: code})
}

func (f *e2eArray) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	volPath := "/api/instances/Volume::" + e2eVolID
	switch {
	case r.Method == http.MethodGet && p == "/api/instances/System::"+e2eSys+"/relationships/Sdc":
		f.getSdcCalls++
		if f.failGetSdcFrom > 0 && f.getSdcCalls >= f.failGetSdcFrom {
			f.writeErr(w, http.StatusInternalServerError, "induced GetSdc failure")
			return
		}
		_ = json.NewEncoder(w).Encode(f.sdcs)
	case r.Method == http.MethodGet && p == volPath:
		m := f.mappings
		if f.getVolMappings != nil {
			m = f.getVolMappings
		}
		_ = json.NewEncoder(w).Encode(siotypes.Volume{ID: e2eVolID, Name: "k8s-pvc-vol", MappedSdcInfo: m})
	case r.Method == http.MethodPost && p == volPath+"/action/removeMappedSdc":
		var body siotypes.UnmapVolumeSdcParam
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.actions = append(f.actions, "unmap:"+body.SdcID)
		if e, ok := f.unmapFail[body.SdcID]; ok {
			f.writeErr(w, e.code, e.msg)
			return
		}
		kept := f.mappings[:0:0]
		found := false
		for _, m := range f.mappings {
			if m.SdcID == body.SdcID {
				found = true
				continue
			}
			kept = append(kept, m)
		}
		if !found {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(siotypes.Error{
				Message: "Volume not mapped to SDC", HTTPStatusCode: http.StatusInternalServerError,
				ErrorCode: 84, // PowerFlex 3.x: volume not mapped
			})
			return
		}
		f.mappings = kept
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && p == volPath+"/action/addMappedSdc":
		var body siotypes.MapVolumeSdcParam
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.actions = append(f.actions, "map:"+body.SdcID)
		if body.AllowMultipleMappings != "true" {
			for _, m := range f.mappings {
				if m.SdcID != body.SdcID {
					f.writeErr(w, http.StatusInternalServerError, "Only a single SDC may be mapped to this volume at a time")
					return
				}
			}
		}
		for _, s := range f.sdcs {
			if s.ID == body.SdcID {
				f.mappings = append(f.mappings, e2eMap(s))
			}
		}
		w.WriteHeader(http.StatusOK)
	default:
		f.actions = append(f.actions, "unexpected:"+r.Method+" "+p)
		f.writeErr(w, http.StatusNotFound, "not found")
	}
}

func e2eService(t *testing.T, f *e2eArray) *service {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client, err := sio.NewClientWithArgs(srv.URL, "4.0", 0, true, false, "")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	client.SetToken("test-token")
	sys := sio.NewSystem(client)
	sys.System.ID = e2eSys
	return &service{
		adminClients:            map[string]*sio.Client{e2eSys: client},
		systems:                 map[string]*sio.System{e2eSys: sys},
		opts:                    Opts{arrays: map[string]*ArrayConnectionData{}},
		volumePrefixToSystems:   map[string][]string{},
		connectedSystemNameToID: map[string]string{},
	}
}

func e2ePublishReq(node siotypes.Sdc, mode csi.VolumeCapability_AccessMode_Mode, block bool) *csi.ControllerPublishVolumeRequest {
	vc := &csi.VolumeCapability{AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode}}
	if block {
		vc.AccessType = &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}
	} else {
		vc.AccessType = &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}}
	}
	return &csi.ControllerPublishVolumeRequest{VolumeId: e2eCsiVol, NodeId: node.SdcGUID, VolumeCapability: vc, VolumeContext: map[string]string{}}
}

func TestStaleSdcMappingPublishUnpublish(t *testing.T) {
	rwo := csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
	all := []siotypes.Sdc{e2eAOld, e2eANew, e2eB, e2eCDown}
	m := func(s ...*siotypes.MappedSdcInfo) []*siotypes.MappedSdcInfo { return s }
	nvme := &siotypes.MappedSdcInfo{SdcID: "e000000000000009", SdcName: "nvme-host", HostType: "NVMeHost"}
	aOld36 := e2eMap(e2eAOld)
	aOld36.HostType = "" // PowerFlex 3.6

	tests := []struct {
		name         string
		op           string // "publish" | "unpublish"
		node         siotypes.Sdc
		mode         csi.VolumeCapability_AccessMode_Mode
		block        bool
		sdcs         []siotypes.Sdc
		mappings     []*siotypes.MappedSdcInfo
		getVolView   []*siotypes.MappedSdcInfo
		failGetSdc   int
		unmapFail    map[string]e2eFailure
		wantCode     codes.Code
		wantErr      string
		wantActions  string
		wantMappings string
	}{
		// ---------------- ControllerUnpublishVolume ----------------
		{
			name: "U1 unpublish: node SDC gone from array -> OK", op: "unpublish", node: e2eANew,
			sdcs: []siotypes.Sdc{e2eB}, wantCode: codes.OK,
		},
		{
			name: "U2 unpublish: SDC list unreadable -> still error", op: "unpublish", node: e2eANew,
			sdcs: all, failGetSdc: 1, wantCode: codes.Internal,
		},
		{
			name: "U3 unpublish from re-imaged node removes its stale registration", op: "unpublish", node: e2eANew,
			sdcs: all, mappings: m(e2eMap(e2eAOld)), wantCode: codes.OK,
			wantActions: "unmap:" + e2eAOld.ID,
		},
		{
			name: "U4 unpublish leaves another node's live mapping alone", op: "unpublish", node: e2eANew,
			sdcs: all, mappings: m(e2eMap(e2eANew), e2eMap(e2eB)), wantCode: codes.OK,
			wantActions: "unmap:" + e2eANew.ID, wantMappings: e2eB.ID,
		},
		{
			name: "U5 unpublish ignores powered-off node mapping", op: "unpublish", node: e2eANew,
			sdcs: all, mappings: m(e2eMap(e2eCDown)), wantCode: codes.OK,
			wantMappings: e2eCDown.ID,
		},
		{
			name: "U6 unpublish: stale cleanup failure does not block detach", op: "unpublish", node: e2eANew,
			sdcs: all, mappings: m(e2eMap(e2eAOld), e2eMap(e2eANew)),
			unmapFail: map[string]e2eFailure{e2eAOld.ID: {500, "induced unmap failure"}}, wantCode: codes.OK,
			wantActions: "unmap:" + e2eAOld.ID + ",unmap:" + e2eANew.ID, wantMappings: e2eAOld.ID,
		},
		{
			name: "U7 unpublish from B does not clean A's stale registration", op: "unpublish", node: e2eB,
			sdcs: all, mappings: m(e2eMap(e2eAOld), e2eMap(e2eB)), wantCode: codes.OK,
			wantActions: "unmap:" + e2eB.ID, wantMappings: e2eAOld.ID,
		},

		// ---------------- ControllerPublishVolume (RWO) ----------------
		{
			name: "P1 RWO publish to B clears re-imaged A's stale mapping", op: "publish", node: e2eB, mode: rwo,
			sdcs: all, mappings: m(e2eMap(e2eAOld)), wantCode: codes.OK,
			wantActions: "unmap:" + e2eAOld.ID + ",map:" + e2eB.ID, wantMappings: e2eB.ID,
		},
		{
			name: "P2 RWO publish back to re-imaged A itself", op: "publish", node: e2eANew, mode: rwo,
			sdcs: all, mappings: m(e2eMap(e2eAOld)), wantCode: codes.OK,
			wantActions: "unmap:" + e2eAOld.ID + ",map:" + e2eANew.ID, wantMappings: e2eANew.ID,
		},
		{
			name: "P3 RWO publish blocked by live SDC on another node", op: "publish", node: e2eANew, mode: rwo,
			sdcs: all, mappings: m(e2eMap(e2eB)), wantCode: codes.FailedPrecondition,
			wantErr: "volume already published to SDC id: " + e2eB.ID, wantMappings: e2eB.ID,
		},
		{
			name: "P4 RWO publish blocked by powered-off node", op: "publish", node: e2eB, mode: rwo,
			sdcs: all, mappings: m(e2eMap(e2eCDown)), wantCode: codes.FailedPrecondition,
			wantErr: "volume already published to SDC id: " + e2eCDown.ID, wantMappings: e2eCDown.ID,
		},
		{
			name: "P5 stale + live mapping: stale removed, still blocked by live", op: "publish", node: e2eANew, mode: rwo,
			sdcs: all, mappings: m(e2eMap(e2eAOld), e2eMap(e2eB)), wantCode: codes.FailedPrecondition,
			wantErr: e2eB.ID, wantActions: "unmap:" + e2eAOld.ID, wantMappings: e2eB.ID,
		},
		{
			name: "P6 SDC list fails at conflict check -> original FailedPrecondition", op: "publish", node: e2eB, mode: rwo,
			sdcs: all, mappings: m(e2eMap(e2eAOld)), failGetSdc: 3, wantCode: codes.FailedPrecondition,
			wantErr: "volume already published to SDC id: " + e2eAOld.ID, wantMappings: e2eAOld.ID,
		},
		{
			name: "P7 unmap of stale mapping fails -> Internal, no map", op: "publish", node: e2eB, mode: rwo,
			sdcs: all, mappings: m(e2eMap(e2eAOld)),
			unmapFail: map[string]e2eFailure{e2eAOld.ID: {500, "induced unmap failure"}}, wantCode: codes.Internal,
			wantActions: "unmap:" + e2eAOld.ID, wantMappings: e2eAOld.ID,
		},
		{
			name: "P8 stale mapping already gone on array (not mapped) -> continue", op: "publish", node: e2eB, mode: rwo,
			sdcs: all, mappings: nil, getVolView: m(e2eMap(e2eAOld)), wantCode: codes.OK,
			wantActions: "unmap:" + e2eAOld.ID + ",map:" + e2eB.ID, wantMappings: e2eB.ID,
		},
		{
			name: "P9 NVMe mapping still blocks SDC RWO publish", op: "publish", node: e2eB, mode: rwo,
			sdcs: all, mappings: m(nvme), wantCode: codes.FailedPrecondition,
			wantErr: "volume already published", wantMappings: nvme.SdcID,
		},
		{
			name: "P10 PowerFlex 3.6 (empty hostType) stale mapping cleared", op: "publish", node: e2eB, mode: rwo,
			sdcs: all, mappings: m(aOld36), wantCode: codes.OK,
			wantActions: "unmap:" + e2eAOld.ID + ",map:" + e2eB.ID, wantMappings: e2eB.ID,
		},
		{
			name: "P11 RWX block publish unaffected", op: "publish", node: e2eANew,
			mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, block: true,
			sdcs: all, mappings: m(e2eMap(e2eB)), wantCode: codes.OK,
			wantActions: "map:" + e2eANew.ID, wantMappings: e2eB.ID + "," + e2eANew.ID,
		},
		{
			name: "P12 idempotent: already mapped to target", op: "publish", node: e2eB, mode: rwo,
			sdcs: all, mappings: m(e2eMap(e2eB)), wantCode: codes.OK, wantMappings: e2eB.ID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &e2eArray{
				sdcs:           tc.sdcs,
				mappings:       append([]*siotypes.MappedSdcInfo(nil), tc.mappings...),
				getVolMappings: tc.getVolView,
				failGetSdcFrom: tc.failGetSdc,
				unmapFail:      tc.unmapFail,
			}
			s := e2eService(t, f)
			var err error
			if tc.op == "publish" {
				_, err = s.ControllerPublishVolume(context.Background(), e2ePublishReq(tc.node, tc.mode, tc.block))
			} else {
				_, err = s.ControllerUnpublishVolume(context.Background(),
					&csi.ControllerUnpublishVolumeRequest{VolumeId: e2eCsiVol, NodeId: tc.node.SdcGUID})
			}
			ids := make([]string, 0, len(f.mappings))
			for _, mm := range f.mappings {
				ids = append(ids, mm.SdcID)
			}
			got := status.Code(err)
			t.Logf("code=%v GetSdcCalls=%d actions=[%s] finalMappings=[%s] err=%v",
				got, f.getSdcCalls, strings.Join(f.actions, ","), strings.Join(ids, ","), err)

			if got != tc.wantCode {
				t.Errorf("code: got %v, want %v (err=%v)", got, tc.wantCode, err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("error %v does not contain %q", err, tc.wantErr)
			}
			if a := strings.Join(f.actions, ","); a != tc.wantActions {
				t.Errorf("actions: got [%s], want [%s]", a, tc.wantActions)
			}
			if fm := strings.Join(ids, ","); fm != tc.wantMappings {
				t.Errorf("final mappings: got [%s], want [%s]", fm, tc.wantMappings)
			}
		})
	}
}
