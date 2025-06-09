/*
 * Copyright 2023 LiveKit, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package types

import (
	"fmt"
	"strings"
	"sync"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"golang.org/x/exp/slices"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/observability/roomobs"
)

type ICEConnectionType int

const (
	// this is in ICE priority highest -> lowest ordering
	// WARNING: Keep this ordering as it is used to find lowest priority connection type.
	ICEConnectionTypeUnknown ICEConnectionType = iota
	ICEConnectionTypeUDP
	ICEConnectionTypeTCP
	ICEConnectionTypeTURN
)

func (i ICEConnectionType) String() string {
	switch i {
	case ICEConnectionTypeUnknown:
		return "unknown"
	case ICEConnectionTypeUDP:
		return "udp"
	case ICEConnectionTypeTCP:
		return "tcp"
	case ICEConnectionTypeTURN:
		return "turn"
	default:
		return "unknown"
	}
}

func (i ICEConnectionType) ReporterType() roomobs.ConnectionType {
	switch i {
	case ICEConnectionTypeUnknown:
		return roomobs.ConnectionTypeUndefined
	case ICEConnectionTypeUDP:
		return roomobs.ConnectionTypeUDP
	case ICEConnectionTypeTCP:
		return roomobs.ConnectionTypeTCP
	case ICEConnectionTypeTURN:
		return roomobs.ConnectionTypeTurn
	default:
		return roomobs.ConnectionTypeUndefined
	}
}

// --------------------------------------------

type ICECandidateExtended struct {
	// only one of local or remote is set. This is due to type foo in Pion
	Local         *webrtc.ICECandidate
	Remote        ice.Candidate
	SelectedOrder int
	Filtered      bool
	Trickle       bool
}

// --------------------------------------------

type ICEConnectionInfo struct {
	Local     []*ICECandidateExtended
	Remote    []*ICECandidateExtended
	Transport livekit.SignalTarget
	Type      ICEConnectionType
}

func (i *ICEConnectionInfo) HasCandidates() bool {
	return len(i.Local) > 0 || len(i.Remote) > 0
}

func ICEConnectionInfosType(infos []*ICEConnectionInfo) ICEConnectionType {
	for _, info := range infos {
		if info.Type != ICEConnectionTypeUnknown {
			return info.Type
		}
	}
	return ICEConnectionTypeUnknown
}

// --------------------------------------------

type ICEConnectionDetails struct {
	ICEConnectionInfo
	lock          sync.Mutex
	selectedCount int
	logger        logger.Logger
}

func NewICEConnectionDetails(transport livekit.SignalTarget, l logger.Logger) *ICEConnectionDetails {
	d := &ICEConnectionDetails{
		ICEConnectionInfo: ICEConnectionInfo{
			Transport: transport,
			Type:      ICEConnectionTypeUnknown,
		},
		logger: l,
	}
	return d
}

func (d *ICEConnectionDetails) GetInfo() *ICEConnectionInfo {
	d.lock.Lock()
	defer d.lock.Unlock()
	info := &ICEConnectionInfo{
		Transport: d.Transport,
		Type:      d.Type,
		Local:     make([]*ICECandidateExtended, 0, len(d.Local)),
		Remote:    make([]*ICECandidateExtended, 0, len(d.Remote)),
	}
	for _, c := range d.Local {
		info.Local = append(info.Local, &ICECandidateExtended{
			Local:         c.Local,
			Filtered:      c.Filtered,
			SelectedOrder: c.SelectedOrder,
			Trickle:       c.Trickle,
		})
	}
	for _, c := range d.Remote {
		info.Remote = append(info.Remote, &ICECandidateExtended{
			Remote:        c.Remote,
			Filtered:      c.Filtered,
			SelectedOrder: c.SelectedOrder,
			Trickle:       c.Trickle,
		})
	}
	return info
}

func (d *ICEConnectionDetails) GetConnectionType() ICEConnectionType {
	d.lock.Lock()
	defer d.lock.Unlock()

	return d.Type
}

func (d *ICEConnectionDetails) AddLocalCandidate(c *webrtc.ICECandidate, filtered, trickle bool) {
	d.lock.Lock()
	defer d.lock.Unlock()
	compFn := func(e *ICECandidateExtended) bool {
		return isCandidateEqualTo(e.Local, c)
	}
	if slices.ContainsFunc(d.Local, compFn) {
		return
	}
	d.Local = append(d.Local, &ICECandidateExtended{
		Local:    c,
		Filtered: filtered,
		Trickle:  trickle,
	})
}

func (d *ICEConnectionDetails) AddLocalICECandidate(c ice.Candidate, filtered, trickle bool) {
	candidate, err := unmarshalCandidate(c)
	if err != nil {
		d.logger.Errorw("could not unmarshal ice candidate", err, "candidate", c)
		return
	}

	d.AddLocalCandidate(candidate, filtered, trickle)
}

func (d *ICEConnectionDetails) AddRemoteCandidate(c webrtc.ICECandidateInit, filtered, trickle, canUpdate bool) {
	candidate, err := unmarshalICECandidate(c)
	if err != nil {
		d.logger.Errorw("could not unmarshal candidate", err, "candidate", c)
		return
	}
	d.AddRemoteICECandidate(candidate, filtered, trickle, canUpdate)
}

func (d *ICEConnectionDetails) AddRemoteICECandidate(candidate ice.Candidate, filtered, trickle, canUpdate bool) {
	if candidate == nil {
		// end-of-candidates candidate
		return
	}

	d.lock.Lock()
	defer d.lock.Unlock()
	indexFn := func(e *ICECandidateExtended) bool {
		return isICECandidateEqualTo(e.Remote, candidate)
	}
	if idx := slices.IndexFunc(d.Remote, indexFn); idx != -1 {
		if canUpdate {
			d.Remote[idx].Filtered = filtered
			d.Remote[idx].Trickle = trickle
		}
		return
	}
	d.Remote = append(d.Remote, &ICECandidateExtended{
		Remote:   candidate,
		Filtered: filtered,
		Trickle:  trickle,
	})
	d.updateConnectionTypeLocked()
}

func (d *ICEConnectionDetails) Clear() {
	d.lock.Lock()
	defer d.lock.Unlock()
	d.Local = nil
	d.Remote = nil
	d.Type = ICEConnectionTypeUnknown
}

func (d *ICEConnectionDetails) SetSelectedPair(pair *webrtc.ICECandidatePair) {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.selectedCount++

	remoteIdx := slices.IndexFunc(d.Remote, func(e *ICECandidateExtended) bool {
		return isICECandidateEqualToCandidate(e.Remote, pair.Remote)
	})
	if remoteIdx < 0 {
		// it's possible for prflx candidates to be generated by Pion, we'll add them
		candidate, err := unmarshalICECandidate(pair.Remote.ToJSON())
		if err != nil {
			d.logger.Errorw("could not unmarshal remote candidate", err, "candidate", pair.Remote)
			return
		}
		if candidate == nil {
			return
		}
		d.Remote = append(d.Remote, &ICECandidateExtended{
			Remote:   candidate,
			Filtered: false,
			Trickle:  false,
		})
		remoteIdx = len(d.Remote) - 1
	}
	d.Remote[remoteIdx].SelectedOrder = d.selectedCount
	d.updateConnectionTypeLocked()

	localIdx := slices.IndexFunc(d.Local, func(e *ICECandidateExtended) bool {
		return isCandidateEqualTo(e.Local, pair.Local)
	})
	if localIdx < 0 {
		d.logger.Errorw("could not match local candidate", nil, "local", pair.Local)
		// should not happen
		return
	}
	d.Local[localIdx].SelectedOrder = d.selectedCount
}

func (d *ICEConnectionDetails) updateConnectionTypeLocked() {
	highestSelectedOrder := -1
	var selectedRemoteCandidate *ICECandidateExtended
	for _, remote := range d.Remote {
		if remote.SelectedOrder == 0 {
			continue
		}

		if remote.SelectedOrder > highestSelectedOrder {
			highestSelectedOrder = remote.SelectedOrder
			selectedRemoteCandidate = remote
		}
	}

	if selectedRemoteCandidate == nil {
		return
	}

	remoteCandidate := selectedRemoteCandidate.Remote
	switch remoteCandidate.NetworkType() {
	case ice.NetworkTypeUDP4, ice.NetworkTypeUDP6:
		d.Type = ICEConnectionTypeUDP

	case ice.NetworkTypeTCP4, ice.NetworkTypeTCP6:
		d.Type = ICEConnectionTypeTCP
	}

	switch remoteCandidate.Type() {
	case ice.CandidateTypeRelay:
		d.Type = ICEConnectionTypeTURN

	case ice.CandidateTypePeerReflexive:
		// if the remote relay candidate pings us *before* we get a relay candidate,
		// Pion would have created a prflx candidate with the same address as the relay candidate.
		// to report an accurate connection type, we'll compare to see if existing relay candidates match
		for _, other := range d.Remote {
			or := other.Remote
			if or.Type() == ice.CandidateTypeRelay &&
				remoteCandidate.Address() == or.Address() &&
				// NOTE: port is not compared as relayed address  reported by TURN ALLOCATE from
				// pion/turn server -> client and later sent from client -> server via ICE Trickle does not
				// match port of `prflx` candidate learnt via TURN path. TODO-INVESTIGATE: how and why doesn't
				// port match?
				//remoteCanddiate.Port() == or.Port() &&
				remoteCandidate.NetworkType().NetworkShort() == or.NetworkType().NetworkShort() {
				d.Type = ICEConnectionTypeTURN
				break
			}
		}
	}
}

// -------------------------------------------------------------

func isCandidateEqualTo(c1, c2 *webrtc.ICECandidate) bool {
	if c1 == nil && c2 == nil {
		return true
	}
	if (c1 == nil && c2 != nil) || (c1 != nil && c2 == nil) {
		return false
	}
	return c1.Typ == c2.Typ &&
		c1.Protocol == c2.Protocol &&
		c1.Address == c2.Address &&
		c1.Port == c2.Port &&
		c1.Foundation == c2.Foundation &&
		c1.Priority == c2.Priority &&
		c1.RelatedAddress == c2.RelatedAddress &&
		c1.RelatedPort == c2.RelatedPort &&
		c1.TCPType == c2.TCPType
}

func isICECandidateEqualTo(c1, c2 ice.Candidate) bool {
	if c1 == nil && c2 == nil {
		return true
	}
	if (c1 == nil && c2 != nil) || (c1 != nil && c2 == nil) {
		return false
	}
	return c1.Type() == c2.Type() &&
		c1.NetworkType() == c2.NetworkType() &&
		c1.Address() == c2.Address() &&
		c1.Port() == c2.Port() &&
		c1.Foundation() == c2.Foundation() &&
		c1.Priority() == c2.Priority() &&
		c1.RelatedAddress().Equal(c2.RelatedAddress()) &&
		c1.TCPType() == c2.TCPType()
}

func isICECandidateEqualToCandidate(c1 ice.Candidate, c2 *webrtc.ICECandidate) bool {
	if c1 == nil && c2 == nil {
		return true
	}
	if (c1 == nil && c2 != nil) || (c1 != nil && c2 == nil) {
		return false
	}
	return c1.Type().String() == c2.Typ.String() &&
		c1.NetworkType().NetworkShort() == c2.Protocol.String() &&
		c1.Address() == c2.Address &&
		c1.Port() == int(c2.Port) &&
		c1.Foundation() == c2.Foundation &&
		c1.Priority() == c2.Priority &&
		c1.TCPType().String() == c2.TCPType
}

func unmarshalICECandidate(c webrtc.ICECandidateInit) (ice.Candidate, error) {
	candidateValue := strings.TrimPrefix(c.Candidate, "candidate:")
	if candidateValue == "" {
		return nil, nil
	}

	candidate, err := ice.UnmarshalCandidate(candidateValue)
	if err != nil {
		return nil, err
	}

	return candidate, nil
}

func unmarshalCandidate(i ice.Candidate) (*webrtc.ICECandidate, error) {
	var typ webrtc.ICECandidateType
	switch i.Type() {
	case ice.CandidateTypeHost:
		typ = webrtc.ICECandidateTypeHost
	case ice.CandidateTypeServerReflexive:
		typ = webrtc.ICECandidateTypeSrflx
	case ice.CandidateTypePeerReflexive:
		typ = webrtc.ICECandidateTypePrflx
	case ice.CandidateTypeRelay:
		typ = webrtc.ICECandidateTypeRelay
	default:
		return nil, fmt.Errorf("unknown candidate type: %s", i.Type())
	}

	var protocol webrtc.ICEProtocol
	switch strings.ToLower(i.NetworkType().NetworkShort()) {
	case "udp":
		protocol = webrtc.ICEProtocolUDP
	case "tcp":
		protocol = webrtc.ICEProtocolTCP
	default:
		return nil, fmt.Errorf("unknown network type: %s", i.NetworkType())
	}

	c := webrtc.ICECandidate{
		Foundation: i.Foundation(),
		Priority:   i.Priority(),
		Address:    i.Address(),
		Protocol:   protocol,
		Port:       uint16(i.Port()),
		Component:  i.Component(),
		Typ:        typ,
		TCPType:    i.TCPType().String(),
	}

	if i.RelatedAddress() != nil {
		c.RelatedAddress = i.RelatedAddress().Address
		c.RelatedPort = uint16(i.RelatedAddress().Port)
	}

	return &c, nil
}

func IsCandidateMDNS(candidate webrtc.ICECandidateInit) bool {
	c, err := unmarshalICECandidate(candidate)
	if err != nil {
		return false
	}

	return IsICECandidateMDNS(c)
}

func IsICECandidateMDNS(candidate ice.Candidate) bool {
	if candidate == nil {
		// end-of-candidates candidate
		return false
	}

	return strings.HasSuffix(candidate.Address(), ".local")
}
