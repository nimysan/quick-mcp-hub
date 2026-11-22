package feishuweb

import (
	"net/url"
	"strings"
)

// Gate 判定一篇文档能不能读。返回空串表示放行，否则是拒读原因。
//
// 规则（feishu-auto-verify.md §3b，实测得出）：只有
// copyable===true && exportable===true && isForceSetSecurity!==true 才读。
// copyable / exportable 缺失一律按 permission_unknown 拒读 —— 前端改版后
// 字段没了，宁可拒绝也不静默放行。
//
// 扩展里的 extract.js 先跑同一套判定（判定在先、读取在后，拒读时不碰块树）；
// 这里在 mcp-hub 侧再判一次，扩展哪天出错把正文带回来了也不会漏出去。
// 没有任何参数能跳过它。
func Gate(f *Flags) string {
	if f == nil || f.Copyable == nil || f.Exportable == nil {
		return ReasonPermissionUnknown
	}
	if !*f.Copyable {
		return ReasonCopyRestricted
	}
	if !*f.Exportable {
		return ReasonExportRestricted
	}
	if f.IsForceSetSecurity != nil && *f.IsForceSetSecurity {
		return ReasonSecurityRestricted
	}
	return ""
}

// ReasonText 把拒读原因说成人话，给模型和用户看。
func ReasonText(reason string) string {
	switch reason {
	case ReasonCopyRestricted:
		return "这篇文档的所有者禁止了复制，按规则不读取"
	case ReasonExportRestricted:
		return "这篇文档的所有者禁止了导出，按规则不读取"
	case ReasonSecurityRestricted:
		return "这篇文档开了强制安全设置，按规则不读取"
	case ReasonPermissionUnknown:
		return "判断不了这篇文档是否允许复制/导出（飞书页面可能改版了），按规则不读取"
	}
	return reason
}

// 允许的飞书 / Lark 域名后缀。和扩展 manifest 的 host_permissions 一致。
var allowedSuffixes = []string{".feishu.cn", ".larksuite.com", ".larkoffice.com"}

// ValidateDocURL 检查一个文档地址，返回规整后的地址。
//
// 只接受 https 的 docx / wiki 页面，主机必须是飞书或 Lark 的子域名。
// 电子表格、多维表格、幻灯片没有 PageMain 块树，直接说不支持，
// 免得白开一个标签页。
func ValidateDocURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return "", argErr("只接受 https 的飞书文档地址")
	}
	host := strings.ToLower(u.Hostname())
	if !AllowedHost(host) {
		return "", argErr("%s 不是飞书或 Lark 的域名", host)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[1] == "" {
		return "", argErr("地址里没有文档 token")
	}
	switch parts[0] {
	case "docx", "wiki":
	case "sheets", "base", "slides", "mindnotes", "file", "drive":
		return "", &StatusErr{Status: StatusUnsupportedType, Msg: parts[0] + " 类型的页面暂不支持，只支持云文档（docx）和知识库（wiki）"}
	default:
		return "", argErr("不认识的飞书页面类型 %q，只支持 docx 和 wiki", parts[0])
	}
	if !validToken(parts[1]) {
		return "", argErr("文档 token 格式不对")
	}
	// 只留 scheme、主机和前两段路径：查询串里可能有分享来源之类的追踪参数，
	// 也不该由调用方往页面里塞别的东西。
	return "https://" + host + "/" + parts[0] + "/" + parts[1], nil
}

// AllowedHost 报告主机是否属于飞书 / Lark。
func AllowedHost(host string) bool {
	for _, s := range allowedSuffixes {
		if strings.HasSuffix(host, s) && len(host) > len(s) {
			return true
		}
	}
	return false
}

func validToken(t string) bool {
	if len(t) < 8 || len(t) > 64 {
		return false
	}
	for _, r := range t {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
