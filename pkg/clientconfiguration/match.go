package clientconfiguration

import (
	"context"
	"errors"

	"github.com/d5/tengo/v2"

	"github.com/livekit/protocol/livekit"
)

type Match interface {
	Match(clientInfo *livekit.ClientInfo) (bool, error)
}

type ScriptMatch struct {
	Expr string
}

// use result of eval script expression for match.
// expression examples:
// protocol bigger than 5 : c.protocol > 5
// browser if firefox: c.browser == "firefox"
// combined rule : c.protocol > 5 && c.browser == "firefox"
func (m *ScriptMatch) Match(clientInfo *livekit.ClientInfo) (bool, error) {
	res, err := tengo.Eval(context.TODO(), m.Expr, map[string]interface{}{"c": &clientObject{info: clientInfo}})
	if err != nil {
		return false, err
	}

	if val, ok := res.(bool); ok {
		return val, nil
	}
	return false, errors.New("invalid match expression result")
}

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
		return &tengo.String{Value: c.info.Sdk.String()}, nil
	case "version":
		return &tengo.String{Value: c.info.Version}, nil
	case "protocol":
		return &tengo.Int{Value: int64(c.info.Protocol)}, nil
	case "os":
		return &tengo.String{Value: c.info.Os}, nil
	case "os_version":
		return &tengo.String{Value: c.info.OsVersion}, nil
	case "device_model":
		return &tengo.String{Value: c.info.DeviceModel}, nil
	case "browser":
		return &tengo.String{Value: c.info.Browser}, nil
	case "browser_version":
		return &tengo.String{Value: c.info.BrowserVersion}, nil
	case "address":
		return &tengo.String{Value: c.info.Address}, nil
	}
	return &tengo.Undefined{}, nil
}
