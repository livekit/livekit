package service

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
)

// sipRulePriority returns sorting priority for dispatch rules. Lower value means higher priority.
func sipRulePriority(info *livekit.SIPDispatchRuleInfo) int32 {
	// In all these cases, prefer pin-protected rules.
	// Thus, the order will be the following:
	// - 0: Direct or Pin (both pin-protected)
	// - 1: Individual (pin-protected)
	// - 100: Direct (open)
	// - 101: Individual (open)
	const (
		last = math.MaxInt32
	)
	// TODO: Maybe allow setting specific priorities for dispatch rules?
	switch rule := info.GetRule().GetRule().(type) {
	default:
		return last
	case *livekit.SIPDispatchRule_DispatchRuleDirect:
		if rule.DispatchRuleDirect.GetPin() != "" {
			return 0
		}
		return 100
	case *livekit.SIPDispatchRule_DispatchRulePin:
		// TODO: If we assume that Pin is optional, this rule type is very similar to Direct. Could remove it?
		return 0
	case *livekit.SIPDispatchRule_DispatchRuleIndividual:
		if rule.DispatchRuleIndividual.GetPin() != "" {
			return 1
		}
		return 101
	}
}

// sipSortRules predictably sorts dispatch rules by priority (first one is highest).
func sipSortRules(rules []*livekit.SIPDispatchRuleInfo) {
	sort.Slice(rules, func(i, j int) bool {
		p1, p2 := sipRulePriority(rules[i]), sipRulePriority(rules[j])
		if p1 < p2 {
			return true
		} else if p1 > p2 {
			return false
		}
		// For predictable sorting order.
		room1, _, _ := sipGetPinAndRoom(rules[i])
		room2, _, _ := sipGetPinAndRoom(rules[j])
		return room1 < room2
	})
}

// sipSelectDispatch takes a list of dispatch rules, and takes the decision which one should be selected.
// It returns an error if there are conflicting rules. Returns nil if no rules match.
func sipSelectDispatch(rules []*livekit.SIPDispatchRuleInfo, req *rpc.EvaluateSIPDispatchRulesRequest) (*livekit.SIPDispatchRuleInfo, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	// Sorting will do the selection for us. We already filtered out irrelevant ones in matchSIPDispatchRule.
	sipSortRules(rules)
	byPin := make(map[string]*livekit.SIPDispatchRuleInfo)
	var (
		pinRule  *livekit.SIPDispatchRuleInfo
		openRule *livekit.SIPDispatchRuleInfo
	)
	openCnt := 0
	for _, r := range rules {
		_, pin, err := sipGetPinAndRoom(r)
		if err != nil {
			return nil, err
		}
		if pin == "" {
			openRule = r // last one
			openCnt++
		} else if r2 := byPin[pin]; r2 != nil {
			return nil, fmt.Errorf("Conflicting SIP Dispatch Rules: Same PIN for %q and %q",
				r.SipDispatchRuleId, r2.SipDispatchRuleId)
		} else {
			byPin[pin] = r
			// Pick the first one with a Pin. If Pin was provided in the request, we already filtered the right rules.
			// If not, this rule will just be used to send RequestPin=true flag.
			if pinRule == nil {
				pinRule = r
			}
		}
	}
	if req.GetPin() != "" {
		// If it's still nil that's fine. We will report "no rules matched" later.
		return pinRule, nil
	}
	if pinRule != nil {
		return pinRule, nil
	}
	if openCnt > 1 {
		return nil, fmt.Errorf("Conflicting SIP Dispatch Rules: Matched %d open rules for %q", openCnt, req.CallingNumber)
	}
	return openRule, nil
}

// sipGetPinAndRoom returns a room name/prefix and the pin for a dispatch rule. Just a convenience wrapper.
func sipGetPinAndRoom(info *livekit.SIPDispatchRuleInfo) (room, pin string, err error) {
	// TODO: Could probably add methods on SIPDispatchRuleInfo struct instead.
	switch rule := info.GetRule().GetRule().(type) {
	default:
		return "", "", fmt.Errorf("Unsupported SIP Dispatch Rule: %T", rule)
	case *livekit.SIPDispatchRule_DispatchRuleDirect:
		pin = rule.DispatchRuleDirect.GetPin()
		room = rule.DispatchRuleDirect.GetRoomName()
	case *livekit.SIPDispatchRule_DispatchRulePin:
		pin = rule.DispatchRulePin.GetPin()
		room = rule.DispatchRulePin.GetRoomName()
	case *livekit.SIPDispatchRule_DispatchRuleIndividual:
		pin = rule.DispatchRuleIndividual.GetPin()
		room = rule.DispatchRuleIndividual.GetRoomPrefix()
	}
	return room, pin, nil
}

// sipMatchTrunk finds a SIP Trunk definition matching the request.
// Returns nil if no rules matched or an error if there are conflicting definitions.
func sipMatchTrunk(trunks []*livekit.SIPTrunkInfo, calling, called string) (*livekit.SIPTrunkInfo, error) {
	var (
		selectedTrunk   *livekit.SIPTrunkInfo
		defaultTrunk    *livekit.SIPTrunkInfo
		defaultTrunkCnt int // to error in case there are multiple ones
	)
	for _, tr := range trunks {
		// Do not consider it if regexp doesn't match.
		matches := len(tr.InboundNumbersRegex) == 0
		for _, reStr := range tr.InboundNumbersRegex {
			// TODO: we should cache it
			re, err := regexp.Compile(reStr)
			if err != nil {
				logger.Errorw("cannot parse SIP trunk regexp", err, "trunkID", tr.SipTrunkId)
				continue
			}
			if re.MatchString(calling) {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		if tr.OutboundNumber == "" {
			// Default/wildcard trunk.
			defaultTrunk = tr
			defaultTrunkCnt++
		} else if tr.OutboundNumber == called {
			// Trunk specific to the number.
			if selectedTrunk != nil {
				return nil, fmt.Errorf("Multiple SIP Trunks matched for %q", called)
			}
			selectedTrunk = tr
			// Keep searching! We want to know if there are any conflicting Trunk definitions.
		}
	}
	if selectedTrunk != nil {
		return selectedTrunk, nil
	}
	if defaultTrunkCnt > 1 {
		return nil, fmt.Errorf("Multiple default SIP Trunks matched for %q", called)
	}
	// Could still be nil here.
	return defaultTrunk, nil
}

// sipMatchDispatchRule finds the best dispatch rule matching the request parameters. Returns an error if no rule matched.
// Trunk parameter can be nil, in which case only wildcard dispatch rules will be effective (ones without Trunk IDs).
func sipMatchDispatchRule(trunk *livekit.SIPTrunkInfo, rules []*livekit.SIPDispatchRuleInfo, req *rpc.EvaluateSIPDispatchRulesRequest) (*livekit.SIPDispatchRuleInfo, error) {
	// Trunk can still be nil here in case none matched or were defined.
	// This is still fine, but only in case we'll match exactly one wildcard dispatch rule.
	if len(rules) == 0 {
		return nil, fmt.Errorf("No SIP Dispatch Rules defined")
	}
	// We split the matched dispatch rules into two sets: specific and default (aka wildcard).
	// First, attempt to match any of the specific rules, where we did match the Trunk ID.
	// If nothing matches there - fallback to default/wildcard rules, where no Trunk IDs were mentioned.
	var (
		specificRules []*livekit.SIPDispatchRuleInfo
		defaultRules  []*livekit.SIPDispatchRuleInfo
	)
	// TODO: Apart from Pin, it would be nice to have a NoPin flag.
	//       The way it would work is that we will first list the rules and figure out if at least one has a Pin required.
	//       If it does, we will immediately respond with RequestPin=true. Now, on the SIP bridge side, we will run
	//       audio prompt asking for a Pin. The user will have an options to skip the pin (e.g. press #) and only try
	//       to match no-ping rooms. This will be very useful if only 1 number is available and has to route to both
	//       private and public rooms.
	noPin := false
	sentPin := req.GetPin()
	for _, info := range rules {
		_, rulePin, err := sipGetPinAndRoom(info)
		if err != nil {
			logger.Errorw("Invalid SIP Dispatch Rule", err, "dispatchRuleID", info.SipDispatchRuleId)
			continue
		}
		// Filter heavily on the Pin, so that only relevant rules remain.
		if noPin {
			if rulePin != "" {
				// Skip pin-protected rules if no pin mode requested.
				continue
			}
		} else if sentPin != "" {
			if rulePin == "" {
				// Pin already sent, skip non-pin-protected rules.
				continue
			}
			if sentPin != rulePin {
				// Pin doesn't match. Don't return an error here, just wait for other rule to match (or none at all).
				// Note that we will NOT match non-pin-protected rules, thus it will not fallback to open rules.
				continue
			}
		}
		if len(info.TrunkIds) == 0 {
			// Default/wildcard dispatch rule.
			defaultRules = append(defaultRules, info)
			continue
		}
		// Specific dispatch rules. Require a Trunk associated with the number.
		if trunk == nil {
			continue
		}
		matches := false
		for _, id := range info.TrunkIds {
			if id == trunk.SipTrunkId {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		specificRules = append(specificRules, info)
	}
	best, err := sipSelectDispatch(specificRules, req)
	if err != nil {
		return nil, err
	} else if best != nil {
		return best, nil
	}
	best, err = sipSelectDispatch(defaultRules, req)
	if err != nil {
		return nil, err
	} else if best != nil {
		return best, nil
	}
	if trunk == nil {
		return nil, fmt.Errorf("No SIP Trunk or Dispatch Rules matched for %q", req.CalledNumber)
	}
	return nil, fmt.Errorf("No SIP Dispatch Rules matched for %q", req.CalledNumber)
}

// matchSIPTrunk finds a SIP Trunk definition matching the request.
// Returns nil if no rules matched or an error if there are conflicting definitions.
func (s *IOInfoService) matchSIPTrunk(ctx context.Context, calling, called string) (*livekit.SIPTrunkInfo, error) {
	trunks, err := s.ss.ListSIPTrunk(ctx)
	if err != nil {
		return nil, err
	}
	return sipMatchTrunk(trunks, calling, called)
}

// matchSIPDispatchRule finds the best dispatch rule matching the request parameters. Returns an error if no rule matched.
// Trunk parameter can be nil, in which case only wildcard dispatch rules will be effective (ones without Trunk IDs).
func (s *IOInfoService) matchSIPDispatchRule(ctx context.Context, trunk *livekit.SIPTrunkInfo, req *rpc.EvaluateSIPDispatchRulesRequest) (*livekit.SIPDispatchRuleInfo, error) {
	// Trunk can still be nil here in case none matched or were defined.
	// This is still fine, but only in case we'll match exactly one wildcard dispatch rule.
	rules, err := s.ss.ListSIPDispatchRule(ctx)
	if err != nil {
		return nil, err
	}
	return sipMatchDispatchRule(trunk, rules, req)
}

func (s *IOInfoService) EvaluateSIPDispatchRules(ctx context.Context, req *rpc.EvaluateSIPDispatchRulesRequest) (*rpc.EvaluateSIPDispatchRulesResponse, error) {
	trunk, err := s.matchSIPTrunk(ctx, req.CallingNumber, req.CalledNumber)
	if err != nil {
		return nil, err
	}
	best, err := s.matchSIPDispatchRule(ctx, trunk, req)
	if err != nil {
		return nil, err
	}
	sentPin := req.GetPin()

	from := req.CallingNumber
	if best.HidePhoneNumber {
		// TODO: Decide on the phone masking format.
		//       Maybe keep regional code, but mask all but 4 last digits?
		from = from[len(from)-4:]
	}
	fromName := "Phone " + from

	room, rulePin, err := sipGetPinAndRoom(best)
	if err != nil {
		return nil, err
	}
	if rulePin != "" {
		if sentPin == "" {
			return &rpc.EvaluateSIPDispatchRulesResponse{
				RequestPin: true,
			}, nil
		}
		if rulePin != sentPin {
			// This should never happen in practice, because matchSIPDispatchRule should remove rules with the wrong pin.
			return nil, fmt.Errorf("Incorrect PIN for SIP room")
		}
	} else {
		// Pin was sent, but room doesn't require one. Assume user accidentally pressed phone button.
	}
	switch rule := best.GetRule().GetRule().(type) {
	case *livekit.SIPDispatchRule_DispatchRuleIndividual:
		// TODO: Decide on the suffix. Do we need to escape specific characters?
		room = rule.DispatchRuleIndividual.GetRoomPrefix() + from
	}
	return &rpc.EvaluateSIPDispatchRulesResponse{
		RoomName:            room,
		ParticipantIdentity: fromName,
	}, nil
}

func (s *IOInfoService) GetSIPTrunkAuthentication(ctx context.Context, req *rpc.GetSIPTrunkAuthenticationRequest) (*rpc.GetSIPTrunkAuthenticationResponse, error) {
	trunk, err := s.matchSIPTrunk(ctx, req.From, req.To)
	if err != nil {
		return nil, err
	}
	return &rpc.GetSIPTrunkAuthenticationResponse{
		Username: trunk.Username,
		Password: trunk.Password,
	}, nil
}
