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

// Handling for volume mappings held by superseded SDC registrations.
//
// Kubernetes identifies a node to this driver by the SDC GUID that NodeGetInfo
// reported (CSINode .spec.drivers[].nodeID). When a node is re-imaged, or its
// SDC is re-installed, the SDC registers again with a new GUID and gets a new
// SDC ID. The old SDC object stays on the MDM as "Disconnected" and keeps every
// volume mapping it had.
//
// The external-attacher resolves the node ID from CSINode first, so the
// ControllerUnpublishVolume for the old attachment arrives with the NEW GUID.
// The driver finds the new SDC, sees the volume is not mapped to it and returns
// "already unpublished". The mapping on the old SDC object is never removed,
// and the next ControllerPublishVolume of that RWO volume on any node fails
// with "volume already published to SDC id: <old id>" until someone unmaps the
// volume from (or removes) the old SDC on PowerFlex by hand.
//
// A mapping is treated as stale only when ALL of these hold:
//   - it is an SDC mapping (not NVMe/TCP),
//   - the SDC holding it is reported "Disconnected" by the MDM,
//   - exactly one OTHER SDC, which is "Connected", owns one of its IP addresses.
//
// That combination means the host now runs a different SDC registration from
// the same address, so the old registration cannot be the one doing I/O. Any
// other foreign mapping (a live SDC, a host that is just powered off, an SDC
// unknown to this system) is left alone and still blocks single-node publish.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dell/csmlog"
	"github.com/dell/goscaleio"
	siotypes "github.com/dell/goscaleio/types/v1"
)

const (
	sdcStateConnected    = "Connected"
	sdcStateDisconnected = "Disconnected"

	// sdcHostType is MappedSdcInfo.HostType for SDC mappings. PowerFlex 3.6
	// leaves HostType empty.
	sdcHostType = "SdcHost"
)

// staleSdcMapping is a volume mapping held by a superseded SDC registration.
type staleSdcMapping struct {
	// Stale is the disconnected SDC that still holds the mapping.
	Stale siotypes.Sdc
	// Successor is the connected SDC that now owns one of Stale's IPs.
	Successor siotypes.Sdc
}

// isSdcMapping reports whether a mapping belongs to an SDC host (not NVMe/TCP).
func isSdcMapping(m *siotypes.MappedSdcInfo) bool {
	return m != nil && (m.HostType == "" || m.HostType == sdcHostType)
}

// sdcIPAddrs returns the de-duplicated, non-empty IP addresses of an SDC.
func sdcIPAddrs(sdc *siotypes.Sdc) []string {
	seen := make(map[string]struct{}, len(sdc.SdcIPs)+1)
	ips := make([]string, 0, len(sdc.SdcIPs)+1)
	for _, ip := range append([]string{sdc.SdcIP}, sdc.SdcIPs...) {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if _, ok := seen[ip]; ok {
			continue
		}
		seen[ip] = struct{}{}
		ips = append(ips, ip)
	}
	return ips
}

// findSupersededSdcMappings returns the mappings in mappings that are held by
// superseded SDC registrations, using sdcs (the full SDC list of the system)
// to resolve SDC state and IPs.
//
// A mapping to keepSdcID is never returned. If successorID is not empty, only
// stale mappings whose successor is that SDC are returned.
func findSupersededSdcMappings(
	mappings []*siotypes.MappedSdcInfo,
	sdcs []siotypes.Sdc,
	keepSdcID string,
	successorID string,
) []staleSdcMapping {
	byID := make(map[string]siotypes.Sdc, len(sdcs))
	connectedByIP := make(map[string][]siotypes.Sdc)
	for _, sdc := range sdcs {
		if sdc.ID == "" {
			continue
		}
		byID[sdc.ID] = sdc
		if !strings.EqualFold(sdc.MdmConnectionState, sdcStateConnected) {
			continue
		}
		for _, ip := range sdcIPAddrs(&sdc) {
			connectedByIP[ip] = append(connectedByIP[ip], sdc)
		}
	}

	var stale []staleSdcMapping
	seen := make(map[string]struct{})
	for _, m := range mappings {
		if !isSdcMapping(m) || m.SdcID == "" || m.SdcID == keepSdcID {
			continue
		}
		if _, dup := seen[m.SdcID]; dup {
			continue
		}
		old, ok := byID[m.SdcID]
		if !ok || !strings.EqualFold(old.MdmConnectionState, sdcStateDisconnected) {
			continue
		}
		successor, ok := uniqueSuccessor(old, connectedByIP)
		if !ok {
			continue
		}
		if successorID != "" && successor.ID != successorID {
			continue
		}
		seen[m.SdcID] = struct{}{}
		stale = append(stale, staleSdcMapping{Stale: old, Successor: successor})
	}
	return stale
}

// uniqueSuccessor returns the single connected SDC, other than old, that owns
// one of old's IPs. It returns false if there is none, or more than one (an
// ambiguous address is not proof the old registration is dead).
func uniqueSuccessor(old siotypes.Sdc, connectedByIP map[string][]siotypes.Sdc) (siotypes.Sdc, bool) {
	var found *siotypes.Sdc
	for _, ip := range sdcIPAddrs(&old) {
		for i := range connectedByIP[ip] {
			c := connectedByIP[ip][i]
			if c.ID == old.ID {
				continue
			}
			if old.SdcGUID != "" && strings.EqualFold(c.SdcGUID, old.SdcGUID) {
				continue
			}
			if found != nil && found.ID != c.ID {
				return siotypes.Sdc{}, false
			}
			found = &c
		}
	}
	if found == nil {
		return siotypes.Sdc{}, false
	}
	return *found, true
}

// remainingMappings returns the mappings that were not removed.
func remainingMappings(mappings []*siotypes.MappedSdcInfo, removed []staleSdcMapping) []*siotypes.MappedSdcInfo {
	gone := make(map[string]struct{}, len(removed))
	for _, r := range removed {
		gone[r.Stale.ID] = struct{}{}
	}
	var left []*siotypes.MappedSdcInfo
	for _, m := range mappings {
		if m == nil {
			continue
		}
		if _, ok := gone[m.SdcID]; ok && isSdcMapping(m) {
			continue
		}
		left = append(left, m)
	}
	return left
}

// describeMappings renders mappings for error messages, enriched with SDC
// state from sdcs when available, so an operator can see at a glance which
// SDC is blocking the publish.
func describeMappings(mappings []*siotypes.MappedSdcInfo, sdcs []siotypes.Sdc) string {
	byID := make(map[string]siotypes.Sdc, len(sdcs))
	for _, sdc := range sdcs {
		byID[sdc.ID] = sdc
	}
	parts := make([]string, 0, len(mappings))
	for _, m := range mappings {
		if m == nil {
			continue
		}
		state := "unknown"
		guid := ""
		if sdc, ok := byID[m.SdcID]; ok {
			if sdc.MdmConnectionState != "" {
				state = sdc.MdmConnectionState
			}
			guid = sdc.SdcGUID
		}
		parts = append(parts, fmt.Sprintf("id=%s name=%q ip=%s guid=%s state=%s hostType=%q",
			m.SdcID, m.SdcName, m.SdcIP, guid, state, m.HostType))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// isNotMappedError reports whether an unmap error only says the mapping is
// already gone (for example removed by a concurrent operator action).
func isNotMappedError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not mapped")
}

// listSystemSdcs returns all SDCs of a system.
func (s *service) listSystemSdcs(systemID string) ([]siotypes.Sdc, error) {
	system := s.systems[systemID]
	if system == nil {
		return nil, fmt.Errorf("system %s is not known to the driver", systemID)
	}
	sdcs, err := system.GetSdc()
	if err != nil {
		return nil, fmt.Errorf("listing SDCs of system %s: %w", systemID, err)
	}
	return sdcs, nil
}

// hasForeignSdcMapping reports whether vol has an SDC mapping other than to keepSdcID.
func hasForeignSdcMapping(vol *siotypes.Volume, keepSdcID string) bool {
	if vol == nil {
		return false
	}
	for _, m := range vol.MappedSdcInfo {
		if isSdcMapping(m) && m.SdcID != "" && m.SdcID != keepSdcID {
			return true
		}
	}
	return false
}

// unmapSupersededSdcMappings unmaps vol from every superseded SDC registration
// (see the file comment). sdcs is the system's SDC list; keepSdcID and
// successorID are passed to findSupersededSdcMappings. It returns the
// mappings it removed; on error, the ones removed before the failure.
func (s *service) unmapSupersededSdcMappings(
	ctx context.Context,
	adminClient *goscaleio.Client,
	vol *siotypes.Volume,
	sdcs []siotypes.Sdc,
	keepSdcID string,
	successorID string,
) ([]staleSdcMapping, error) {
	stale := findSupersededSdcMappings(vol.MappedSdcInfo, sdcs, keepSdcID, successorID)
	if len(stale) == 0 {
		return nil, nil
	}
	if adminClient == nil {
		return nil, fmt.Errorf("no PowerFlex client to unmap volume %s", vol.ID)
	}

	target := goscaleio.NewVolume(adminClient)
	target.Volume = vol

	removed := make([]staleSdcMapping, 0, len(stale))
	for _, sm := range stale {
		csmlog.WithContext(ctx).Warnf(
			"volume %s (%s) is mapped to superseded SDC %s (guid=%s name=%q ips=%v state=%s); "+
				"SDC %s (guid=%s name=%q) is now connected from the same address. Removing the stale mapping.",
			vol.ID, vol.Name, sm.Stale.ID, sm.Stale.SdcGUID, sm.Stale.Name, sdcIPAddrs(&sm.Stale),
			sm.Stale.MdmConnectionState, sm.Successor.ID, sm.Successor.SdcGUID, sm.Successor.Name)

		err := target.UnmapVolumeSdc(&siotypes.UnmapVolumeSdcParam{SdcID: sm.Stale.ID})
		if err != nil && !isNotMappedError(err) {
			return removed, fmt.Errorf("unmapping volume %s from superseded SDC %s: %w", vol.ID, sm.Stale.ID, err)
		}
		removed = append(removed, sm)
	}
	return removed, nil
}

// sdcHostAbsent reports whether the system definitely has no SDC or NVMe host
// for nodeID. It returns false when the SDC list cannot be read, so a failed
// lookup is never mistaken for "not found".
func (s *service) sdcHostAbsent(systemID, nodeID string) bool {
	sdcs, err := s.listSystemSdcs(systemID)
	if err != nil {
		csmlog.Warnf("cannot verify whether node %s has a host on system %s: %v", nodeID, systemID, err)
		return false
	}
	for _, sdc := range sdcs {
		if strings.EqualFold(sdc.SdcGUID, nodeID) || sdc.Name == nodeID {
			return false
		}
	}
	return true
}
