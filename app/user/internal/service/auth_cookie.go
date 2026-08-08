package service

import (
	"context"
	"net/http"
	"time"

	pb "cwxu-algo/api/user/v1/auth"
	commonauth "cwxu-algo/app/common/utils/auth"

	"github.com/go-kratos/kratos/v2/transport"
)

func setSessionCookie(ctx context.Context, token string) {
	tr, ok := transport.FromServerContext(ctx)
	if !ok {
		return
	}
	cookie := (&http.Cookie{
		Name: commonauth.SessionCookieName, Value: token, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(JWTAccessTTL.Seconds()), Expires: time.Now().Add(JWTAccessTTL),
	}).String()
	tr.ReplyHeader().Add("Set-Cookie", cookie)
}

func clearSessionCookie(ctx context.Context) {
	tr, ok := transport.FromServerContext(ctx)
	if !ok {
		return
	}
	cookie := (&http.Cookie{
		Name: commonauth.SessionCookieName, Value: "", Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		MaxAge: -1, Expires: time.Unix(1, 0),
	}).String()
	tr.ReplyHeader().Add("Set-Cookie", cookie)
}

// Logout 退出登录：清理 session cookie（业务错误/成功均 HTTP 200）
func (s *AuthService) Logout(ctx context.Context, _ *pb.LogoutReq) (*pb.LogoutRes, error) {
	clearSessionCookie(ctx)
	return &pb.LogoutRes{Code: 0, Success: true, Message: "已退出登录"}, nil
}
