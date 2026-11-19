package nginx

// FrontSettings tests. Only the gathering is tested here: each group's values
// belong to the unit that owns it.

import (
	"reflect"
	"testing"
)

// Every group is set, from its owner's defaults, and is a new instance on
// every call, so no two fronts share mutable settings.
func TestDefaultFrontSettingsBuildsFreshOwnerDefaults(t *testing.T) {
	settings := DefaultFrontSettings()
	otherSettings := DefaultFrontSettings()
	if settings == otherSettings {
		t.Fatalf("DefaultFrontSettings returned one instance twice")
	}

	settingsValue := reflect.ValueOf(settings).Elem()
	otherSettingsValue := reflect.ValueOf(otherSettings).Elem()
	for i := range settingsValue.NumField() {
		fieldName := settingsValue.Type().Field(i).Name
		field := settingsValue.Field(i)
		if field.Kind() != reflect.Pointer {
			t.Errorf("%s is not a pointer to its owner's settings", fieldName)
			continue
		}
		if field.IsNil() {
			t.Errorf("%s is nil", fieldName)
			continue
		}
		// distinct allocations of a zero-size type may share an address
		if field.Type().Elem().Size() == 0 {
			continue
		}
		if field.Pointer() == otherSettingsValue.Field(i).Pointer() {
			t.Errorf("%s is shared between calls", fieldName)
		}
	}

	ownerDefaults := []struct {
		fieldName string
		got       any
		want      any
	}{
		{fieldName: "Tls", got: settings.Tls, want: DefaultTlsSettings()},
		{fieldName: "Forward", got: settings.Forward, want: DefaultForwardSettings()},
		{fieldName: "Body", got: settings.Body, want: DefaultBodySettings()},
		{fieldName: "Limit", got: settings.Limit, want: DefaultLimitSettings()},
		{fieldName: "Http", got: settings.Http, want: DefaultHttpSettings()},
	}
	if len(ownerDefaults) != settingsValue.NumField() {
		t.Fatalf("checked %d groups of %d", len(ownerDefaults), settingsValue.NumField())
	}
	for _, ownerDefault := range ownerDefaults {
		if !reflect.DeepEqual(ownerDefault.got, ownerDefault.want) {
			t.Errorf("%s = %+v, want its owner's defaults %+v", ownerDefault.fieldName, ownerDefault.got, ownerDefault.want)
		}
	}
}
