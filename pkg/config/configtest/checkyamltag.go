package configtest

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"go.uber.org/multierr"
	"google.golang.org/protobuf/proto"
)

var protoMessageType = reflect.TypeOf((*proto.Message)(nil)).Elem()

func checkYAMLTag(v reflect.Type, seen map[reflect.Type]struct{}) error {
	if _, ok := seen[v]; ok {
		return nil
	}
	seen[v] = struct{}{}

	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.Pointer:
		return checkYAMLTag(v.Elem(), seen)
	case reflect.Struct:
		if reflect.PointerTo(v).Implements(protoMessageType) {
			// ignore protobuf messages
			return nil
		}

		var errs error
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)

			if field.Type.Kind() == reflect.Bool {
				// ignore boolean fields
				continue
			}

			if unicode.IsLower([]rune(field.Name)[0]) {
				// ignore private fields
				continue
			}

			if field.Tag.Get("config") == "allowempty" {
				// ignore configured exceptions
				continue
			}

			parts := strings.Split(field.Tag.Get("yaml"), ",")
			if len(parts) == 0 || parts[0] == "-" {
				// ignore unparsed fields
				continue
			}

			if !slices.Contains(parts, "omitempty") && !slices.Contains(parts, "inline") {
				errs = multierr.Append(errs, fmt.Errorf("%s/%s.%s missing omitempty tag", v.PkgPath(), v.Name(), field.Name))
			}

			if err := checkYAMLTag(field.Type, seen); err != nil {
				errs = multierr.Append(errs, err)
			}
		}
		return errs
	default:
		return nil
	}
}

func CheckYAMLTag(config any) error {
	return checkYAMLTag(reflect.TypeOf(config), map[reflect.Type]struct{}{})
}
