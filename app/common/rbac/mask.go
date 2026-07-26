package rbac

import "encoding/base64"

// 权限位图：按 Perm.Bit 打包为小端字节序 bit 数组，base64url（无 padding）编码后放入 JWT `pm` claim。
// 37 个权限 ≈ 5 字节 ≈ 7 字符，token 体积可忽略。

// Encode 权限 code 集合 → base64url 位图。未注册的 code 忽略。
func Encode(codes []string) string {
	maxBit := -1
	for _, p := range registry {
		if p.Bit > maxBit {
			maxBit = p.Bit
		}
	}
	buf := make([]byte, maxBit/8+1)
	any := false
	for _, c := range codes {
		p, ok := byCode[c]
		if !ok {
			continue
		}
		buf[p.Bit/8] |= 1 << (p.Bit % 8)
		any = true
	}
	if !any {
		return base64.RawURLEncoding.EncodeToString(make([]byte, 1))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// Decode base64url 位图 → 权限 code 集合。解码失败返回 ok=false。
func Decode(mask string) (map[string]bool, bool) {
	if mask == "" {
		return nil, false
	}
	buf, err := base64.RawURLEncoding.DecodeString(mask)
	if err != nil {
		return nil, false
	}
	out := make(map[string]bool)
	for _, p := range registry {
		idx := p.Bit / 8
		if idx < len(buf) && buf[idx]&(1<<(p.Bit%8)) != 0 {
			out[p.Code] = true
		}
	}
	return out, true
}

// MaskHas 位图中是否含权限。valid=false 表示位图无法解码（旧 token / 损坏），调用方应走旧字段推导。
func MaskHas(mask, code string) (has bool, valid bool) {
	p, ok := byCode[code]
	if !ok {
		return false, true // 未注册的权限一律 false，但位图本身有效
	}
	if mask == "" {
		return false, false
	}
	buf, err := base64.RawURLEncoding.DecodeString(mask)
	if err != nil {
		return false, false
	}
	idx := p.Bit / 8
	if idx >= len(buf) {
		// 旧位图短于当前注册表：该位视为未授予（新权限对旧 token 默认关闭）
		return false, true
	}
	return buf[idx]&(1<<(p.Bit%8)) != 0, true
}
