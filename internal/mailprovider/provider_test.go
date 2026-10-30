package mailprovider

import "testing"

// TLS 放宽是个安全相关的开关，必须钉住「谁开了它」。
//
// 它的代价是那条连接没有前向保密，所以不能因为某台服务器老就顺手给别人也开。
// 新增服务商时如果不小心复制了这一行，这条用例会失败并要求你说明理由。
func TestOnlyKnownProvidersRelaxTLS(t *testing.T) {
	allowed := map[string]string{
		// 自建 Postfix + Dovecot，只接受静态 RSA 套件（实测 8 个，无 ECDHE、
		// 无 TLS 1.3）。正解是对方加 ECDHE，加上后这个开关自动失效。
		"gotechmail": "mail.gotechcn.cn 只提供静态 RSA 密钥交换",
	}
	for _, p := range All {
		if !p.LegacyRSAKeyExchange {
			continue
		}
		if _, ok := allowed[p.Type]; !ok {
			t.Errorf("%s 开了 LegacyRSAKeyExchange 但没有记录理由。"+
				"这个开关让连接失去前向保密，开之前要实测服务器确实只提供静态 RSA，"+
				"并把结论写进这张表", p.Type)
		}
	}
	for typ := range allowed {
		if _, ok := ByType(typ); !ok {
			t.Errorf("表里记着 %s，但服务商列表里没有它了 —— 清掉这条记录", typ)
		}
	}
}

// 每家服务商都要说清密码字段是什么。QQ 填授权码、GoTech 填登录密码，
// 标签写错会让人拿错东西去试。
func TestEveryProviderExplainsItsSecret(t *testing.T) {
	for _, p := range All {
		if p.SecretLabel == "" {
			t.Errorf("%s 没有 SecretLabel", p.Type)
		}
		if p.SecretHelp == "" {
			t.Errorf("%s 没有 SecretHelp —— 用户不知道该去哪拿这个凭据", p.Type)
		}
		if len(p.Domains) == 0 {
			t.Errorf("%s 没有 Domains，按邮箱域名自动识别就认不出它", p.Type)
		}
		if p.IMAPHost == "" || p.SMTPHost == "" || p.IMAPPort == 0 || p.SMTPPort == 0 {
			t.Errorf("%s 的接入点不完整", p.Type)
		}
	}
}
