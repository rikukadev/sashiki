package main

import "testing"

// majorVersion は @@version 文字列から major を取り、app_user の認証プラグイン
// 選択(major<8 は native、8.0+ は caching_sha2)の分岐に使う(#version-compat)。
func TestMajorVersion(t *testing.T) {
	cases := []struct {
		in    string
		major int
		ok    bool
	}{
		{"8.0.46", 8, true},
		{"8.4.11", 8, true},
		{"9.6.0", 9, true},
		{"26.7.0", 26, true}, // 9.7 以降の calendar versioning(YY.M.P、#285)
		{"26.7.1", 26, true},
		{"5.7.44-log", 5, true},
		{"5.7.44", 5, true},
		{"10.11.5-MariaDB", 10, true}, // MariaDB でも major は取れる
		{"", 0, false},
		{"garbage", 0, false},
		{".5", 0, false},
	}
	for _, c := range cases {
		major, ok := majorVersion(c.in)
		if ok != c.ok || (ok && major != c.major) {
			t.Errorf("majorVersion(%q) = (%d, %v), want (%d, %v)", c.in, major, ok, c.major, c.ok)
		}
	}
}

// pluginForVersion: 5.7 と MariaDB は native、MySQL 8.0+ は caching_sha2(#version-compat)。
func TestPluginForVersion(t *testing.T) {
	const sha2 = "caching_sha2_password"
	const native = "mysql_native_password"
	cases := []struct {
		v    string
		want string
	}{
		{"5.7.44", native},                        // caching_sha2 は 8.0 追加、5.7 に無い
		{"5.7.44-log", native},                    //
		{"8.0.46", sha2},                          //
		{"8.1.0", sha2},                           // 間の版
		{"8.3.0", sha2},                           //
		{"8.4.11", sha2},                          // native 既定 OFF
		{"9.6.0", sha2},                           // native 廃止
		{"26.7.0", sha2},                          // calendar versioning(major 26 ≥ 8)
		{"26.7.1-sashiki", sha2},                  //
		{"10.11.5-MariaDB", native},               // MariaDB は caching_sha2 非対応
		{"11.4.2-MariaDB-1:11.4.2+maria", native}, //
		{"", sha2},                                // 判定不能は既定 caching_sha2
	}
	for _, c := range cases {
		if got := pluginForVersion(c.v); got != c.want {
			t.Errorf("pluginForVersion(%q) = %s, want %s", c.v, got, c.want)
		}
	}
}
