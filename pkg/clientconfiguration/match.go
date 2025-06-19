// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package clientconfiguration

import (
	"errors"
	"fmt"
	"strings"

	"github.com/d5/tengo/v2"
	"github.com/d5/tengo/v2/token"
	"golang.org/x/mod/semver"

	"github.com/livekit/protocol/livekit"
)

type Match interface {
	Match(clientInfo *livekit.ClientInfo) (bool, error)
}

type ScriptMatch struct {
	compiled *tengo.Compiled
}

func NewScriptMatch(expr string) (*ScriptMatch, error) {
	script := tengo.NewScript(fmt.Appendf(nil, "__res__ := (%s)", expr))
	if err := script.Add("c", &clientObject{}); err != nil {
		return nil, err
	}
	compiled, err := script.Compile()
	if err != nil {
		return nil, err
	}
	return &ScriptMatch{compiled}, nil
}

// use result of eval script expression for match.
// expression examples:
// protocol bigger than 5 : c.protocol > 5
// browser if firefox: c.browser == "firefox"
// combined rule : c.protocol > 5 && c.browser == "firefox"
func (m *ScriptMatch) Match(clientInfo *livekit.ClientInfo) (bool, error) {
	clone := m.compiled.Clone()
	if err := clone.Set("c", &clientObject{info: clientInfo}); err != nil {
		return false, err
	}
	if err := clone.Run(); err != nil {
		return false, err
	}

	res := clone.Get("__res__").Value()
	if val, ok := res.(bool); ok {
		return val, nil
	}
	return false, errors.New("invalid match expression result")
}

// ------------------------------------------------

type clientObject struct {
	tengo.ObjectImpl
	info *livekit.ClientInfo
}

func (c *clientObject) TypeName() string {
	return "clientObject"
}

func (c *clientObject) String() string {
	return c.info.String()
}

func (c *clientObject) IndexGet(index tengo.Object) (res tengo.Object, err error) {
	field, ok := index.(*tengo.String)
	if !ok {
		return nil, tengo.ErrInvalidIndexType
	}

	switch field.Value {
	case "sdk":
		return &tengo.String{Value: strings.ToLower(c.info.Sdk.String())}, nil
	case "version":
		return &ruleSdkVersion{sdkVersion: c.info.Version}, nil
	case "protocol":
		return &tengo.Int{Value: int64(c.info.Protocol)}, nil
	case "os":
		return &tengo.String{Value: strings.ToLower(c.info.Os)}, nil
	case "os_version":
		return &tengo.String{Value: c.info.OsVersion}, nil
	case "device_model":
		return &tengo.String{Value: strings.ToLower(c.info.DeviceModel)}, nil
	case "browser":
		return &tengo.String{Value: strings.ToLower(c.info.Browser)}, nil
	case "browser_version":
		return &ruleSdkVersion{sdkVersion: c.info.BrowserVersion}, nil
	case "address":
		return &tengo.String{Value: c.info.Address}, nil
	}
	return &tengo.Undefined{}, nil
}

// ------------------------------------------

type ruleSdkVersion struct {
	tengo.ObjectImpl
	sdkVersion string
}

func (r *ruleSdkVersion) TypeName() string {
	return "sdkVersion"
}

func (r *ruleSdkVersion) String() string {
	return r.sdkVersion
}

func (r *ruleSdkVersion) BinaryOp(op token.Token, rhs tengo.Object) (tengo.Object, error) {
	if rhs, ok := rhs.(*tengo.String); ok {
		cmp := r.compare(rhs.Value)

		isMatch := false
		switch op {
		case token.Greater:
			isMatch = cmp > 0
		case token.GreaterEq:
			isMatch = cmp >= 0
		default:
			return nil, tengo.ErrInvalidOperator
		}

		if isMatch {
			return tengo.TrueValue, nil
		}
		return tengo.FalseValue, nil
	}

	return nil, tengo.ErrInvalidOperator
}

func (r *ruleSdkVersion) Equals(rhs tengo.Object) bool {
	if rhs, ok := rhs.(*tengo.String); ok {
		return r.compare(rhs.Value) == 0
	}

	return false
}

func (r *ruleSdkVersion) compare(rhsSdkVersion string) int {
	if !semver.IsValid("v"+r.sdkVersion) || !semver.IsValid("v"+rhsSdkVersion) {
		// if not valid semver, do string compare
		switch {
		case r.sdkVersion < rhsSdkVersion:
			return -1
		case r.sdkVersion > rhsSdkVersion:
			return 1
		}
	} else {
		return semver.Compare("v"+r.sdkVersion, "v"+rhsSdkVersion)
	}
	return 0
}
