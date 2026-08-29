package problem

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestUserProfileResponseExposesAllTagStats(t *testing.T) {
	fields := (&UserProfileRes{}).ProtoReflect().Descriptor().Fields()
	field := fields.ByName(protoreflect.Name("tag_stats"))
	if field == nil {
		t.Fatal("UserProfileRes must expose tag_stats separately from the top-eight radar")
	}
	if field.Number() != 7 || !field.IsList() || field.Message().FullName() != "api.core.v1.problem.TagScore" {
		t.Fatalf("tag_stats descriptor=%v, want repeated api.core.v1.problem.TagScore field 7", field)
	}
}
