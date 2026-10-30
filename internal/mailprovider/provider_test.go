package mailprovider

import "testing"

// TLS 放宽是个安全相关的开关，必须钉住「谁开了它」。
//
// 它的代价是那条连接没有前向保密，所以不能因为某台服务器老就顺手给别人也开。
// 新增服务商时如果不小心复制了这一行，这条用例会失败并要求你说明理由。
//
// 现在这张表是**空的**，而且应该一直是空的：静态 RSA 这件事已经改成握手
// 失败时自动降级重试（internal/mailbox/tlsconfig.go 的 dialWithFallback），
// 所以不再需要在表里给某台服务器写死。要往这张表里加东西之前，先问一句
// 为什么自动降级不够。
func TestOnlyKnownProvidersRelaxTLS(t *testing.T) {
	allowed := map[string]string{}
	for _, p := range All {
		if !p.LegacyRSAKeyExchange {
			continue
		}
		if _, ok := allowed[p.Type]; !ok {
			t.Errorf("%s 开了 LegacyRSAKeyExchange 但没有记录理由。"+
				"这个开关让连接失去前向保密；而且现在握手失败会自动降级重试，"+
				"所以静态 RSA 的服务器本来就不需要在这里写死", p.Type)
		}
	}
	for typ := range allowed {
		if _, ok := ByType(typ); !ok {
			t.Errorf("表里记着 %s，但服务商列表里没有它了 —— 清掉这条记录", typ)
		}
	}
}

// 每家服务商都要说清密码字段是什么。QQ 填授权码、自建服务器填登录密码，
// 标签写错会让人拿错东西去试。
func TestEveryProviderExplainsItsSecret(t *testing.T) {
	for _, p := range All {
		if p.SecretLabel == "" {
			t.Errorf("%s 没有 SecretLabel", p.Type)
		}
		if p.SecretHelp == "" {
			t.Errorf("%s 没有 SecretHelp —— 用户不知道该去哪拿这个凭据", p.Type)
		}
		// 端口必须有默认值：993 / 465 是标准端口，让用户填端口号是没必要的。
		if p.IMAPPort == 0 || p.SMTPPort == 0 {
			t.Errorf("%s 没有默认端口", p.Type)
		}
	}
}

// 有预设主机的服务商，必须同时有域名用于自动识别；没有预设主机的（通用渠道），
// 必须**不**绑定域名。
//
// 这两件事是一体的：绑了域名就意味着「这个域名的邮箱用这台主机」，而通用渠道
// 的主机是用户填的，绑域名会让自动识别指向一个空主机。
func TestPresetsAndGenericAreConsistent(t *testing.T) {
	for _, p := range All {
		preset := p.IMAPHost != "" && p.SMTPHost != ""
		switch {
		case preset && len(p.Domains) == 0:
			t.Errorf("%s 有预设主机但没有 Domains，按邮箱域名自动识别就认不出它", p.Type)
		case !preset && len(p.Domains) > 0:
			t.Errorf("%s 没有预设主机却绑了域名 %v —— 自动识别会指向一个空主机",
				p.Type, p.Domains)
		case !preset && (p.IMAPHost != "" || p.SMTPHost != ""):
			t.Errorf("%s 只预设了一半主机（IMAP=%q SMTP=%q），要么都给要么都不给",
				p.Type, p.IMAPHost, p.SMTPHost)
		}
	}
}

// 通用渠道必须存在：没有它，自建邮箱的客户就没有任何入口。
func TestGenericChannelExists(t *testing.T) {
	p, ok := ByType("custommail")
	if !ok {
		t.Fatal("没有通用邮箱渠道 custommail —— 自建邮箱的客户无法接入")
	}
	if p.IMAPHost != "" || p.SMTPHost != "" {
		t.Errorf("通用渠道不该预设主机，否则它就只服务一家：IMAP=%q SMTP=%q",
			p.IMAPHost, p.SMTPHost)
	}
	if len(p.Domains) != 0 {
		t.Errorf("通用渠道不该绑定域名：%v", p.Domains)
	}
}

// GuessHosts 是表单自动填写用的，猜错代价小、猜对收益大，但不能猜出垃圾。
func TestGuessHosts(t *testing.T) {
	cases := []struct {
		addr, wantIMAP string
	}{
		// 实测过的域名走提示表。
		{"star.zhang@gotechcn.cn", "mail.gotechcn.cn"},
		{"STAR.ZHANG@GoTechCN.CN", "mail.gotechcn.cn"}, // 域名大小写不敏感
		// 没记录的按最常见的约定猜。
		{"someone@example.com", "mail.example.com"},
		// 填不出地址时不要瞎猜。
		{"nobody", ""},
		{"trailing@", ""},
		{"", ""},
	}
	for _, c := range cases {
		gotIMAP, gotSMTP := GuessHosts(c.addr)
		if gotIMAP != c.wantIMAP {
			t.Errorf("GuessHosts(%q) IMAP = %q，想要 %q", c.addr, gotIMAP, c.wantIMAP)
		}
		if c.wantIMAP != "" && gotSMTP == "" {
			t.Errorf("GuessHosts(%q) 给了 IMAP 却没给 SMTP", c.addr)
		}
	}
}
