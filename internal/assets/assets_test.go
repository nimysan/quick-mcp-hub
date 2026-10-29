package assets

import (
	"bytes"
	"os"
	"testing"
)

// 打包用的那一份和嵌进二进制的这一份必须一致。
//
// 不钉住的话，改了 logo 只跑一遍 logo.py、只更新一个位置，结果是 exe 和安装
// 程序换了图标，而页面的 favicon 和托盘图标还是旧的 —— 而且没人会发现。
func TestEmbeddedIconMatchesPackagingCopy(t *testing.T) {
	ref, err := os.ReadFile("../../packaging/windows/mcp-hub.ico")
	if err != nil {
		t.Fatalf("读打包用的那一份失败: %v", err)
	}
	if !bytes.Equal(Icon, ref) {
		t.Errorf("嵌入的图标和 packaging/windows/mcp-hub.ico 不一致"+
			"（嵌入 %d 字节，打包 %d 字节）。改 logo 时两处都要更新，见 packaging/logo.py",
			len(Icon), len(ref))
	}
}

// .ico 的文件头：保留字 0，类型 1（图标），后面是图标档数。
func TestIconIsAnIcoWithMultipleSizes(t *testing.T) {
	if len(Icon) < 6 {
		t.Fatal("图标数据太短")
	}
	if Icon[0] != 0 || Icon[1] != 0 || Icon[2] != 1 || Icon[3] != 0 {
		t.Fatalf("不是 .ico 文件头: % x", Icon[:4])
	}
	count := int(Icon[4]) | int(Icon[5])<<8
	// 单档 .ico 在小尺寸下会被系统强行缩放，糊成一团；任务栏和托盘用的就是
	// 16/32 那两档，所以多档是必需的，不是讲究。
	if count < 3 {
		t.Errorf("只有 %d 档图标，小尺寸下会被缩放糊掉", count)
	}
}
