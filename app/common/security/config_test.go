package security

import (
	"strings"
	"testing"

	"cwxu-algo/app/common/conf"
	_const "cwxu-algo/app/common/const"
)

func TestConfigureFromServerConfig(t *testing.T) {
	t.Setenv("CWXU_JWT_SECRET", "")
	jwtValue := strings.Repeat("j", 32)
	if err := Configure(&conf.Server{
		JwtSecret: jwtValue,
	}); err != nil {
		t.Fatal(err)
	}
	if _const.JWTSecret() != jwtValue {
		t.Fatal("JWT config value was not installed")
	}
}
