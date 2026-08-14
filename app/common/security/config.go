package security

import (
	"fmt"

	"cwxu-algo/app/common/conf"
	_const "cwxu-algo/app/common/const"
	secretutil "cwxu-algo/app/common/utils/secret"
)

// Configure loads shared security secrets before any service providers start.
func Configure(server *conf.Server) error {
	if server == nil {
		return fmt.Errorf("server configuration is required")
	}
	if err := _const.ConfigureJWTSecret(server.GetJwtSecret()); err != nil {
		return err
	}
	if err := _const.ConfigureJWTKeys(server.GetJwtPrivateKey(), server.GetJwtPublicKey()); err != nil {
		return err
	}
	if err := secretutil.ConfigureKey(server.GetConfigEncryptionKey()); err != nil {
		return err
	}
	return nil
}
