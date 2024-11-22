package configtest

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.uber.org/multierr"
	"google.golang.org/protobuf/proto"
)

var protoMessageType = reflect.TypeOf((*proto.Message)(nil)).Elem()

func checkYAMLTags(t reflect.Type, seen map[reflect.Type]struct{}) error {
	if _, ok := seen[t]; ok {
		return nil
	}
	seen[t] = struct{}{}

	switch t.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.Pointer:
		return checkYAMLTags(t.Elem(), seen)
	case reflect.Struct:
		if reflect.PointerTo(t).Implements(protoMessageType) {
			// ignore protobuf messages
			return nil
		}

		var errs error
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)

			if !field.IsExported() {
				// ignore unexported fields
				continue
			}

			if field.Type.Kind() == reflect.Bool {
				// ignore boolean fields
				continue
			}

			if field.Tag.Get("config") == "allowempty" {
				// ignore configured exceptions
				continue
			}

			parts := strings.Split(field.Tag.Get("yaml"), ",")
			if parts[0] == "-" {
				// ignore unparsed fields
				continue
			}

			if !slices.Contains(parts, "omitempty") && !slices.Contains(parts, "inline") {
				errs = multierr.Append(errs, fmt.Errorf("%s/%s.%s missing omitempty tag", t.PkgPath(), t.Name(), field.Name))
			}

			errs = multierr.Append(errs, checkYAMLTags(field.Type, seen))
		}
		return errs
	default:
		return nil
	}
}

func CheckYAMLTags(config any) error {
	return checkYAMLTags(reflect.TypeOf(config), map[reflect.Type]struct{}{})
}
