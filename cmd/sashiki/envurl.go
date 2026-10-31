package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// connURL はアプリの接続文字列(DATABASE_URL など)を組み立てる。
//
// ユーザー名の `@`(dev@pr-1)は `%40` にエスケープする。そのまま入れると
// ドライバがホストとの区切りと読み違える。host は connect と同じく
// SASHIKI_DB_HOST で上書きできる(手元では proxy のドメイン名が引けないことがある)。
//
// withPassword が false ならパスワードは入れない。標準出力に出すときは
// false にする(cmdEnv と同じ理由: 表示しただけの操作でログに秘密を残さない)。
func connURL(b branchView, db string, withPassword bool) string {
	scheme := "mysql"
	if b.Engine == "postgres" {
		scheme = "postgres"
	}
	u := url.URL{Scheme: scheme, Host: connectHost(b) + ":" + strconv.Itoa(b.Port)}
	if pass := os.Getenv("SASHIKI_DB_PASSWORD"); withPassword && pass != "" {
		u.User = url.UserPassword(b.User, pass)
	} else {
		u.User = url.User(b.User)
	}
	if db != "" {
		u.Path = "/" + db
	}
	return u.String()
}

// dbNameFor は URL に入れるデータベース名。--db、SASHIKI_DB_NAME の順に見る。
// どちらも無ければ、postgres は必ず存在する postgres、mysql は指定なし。
func dbNameFor(b branchView, flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("SASHIKI_DB_NAME"); v != "" {
		return v
	}
	if b.Engine == "postgres" {
		return "postgres"
	}
	return ""
}

// kv は dotenv の 1 行。書き出す順番を保つためにスライスで持つ。
type kv struct{ key, value string }

// writeDotenv は path の dotenv に kvs を書く。同じキーの行は置き換え、
// 無いキーは末尾に足し、それ以外の行(コメントや他のキー)はそのまま残す。
//
// worktree ごとの .env.local のように、人が書いた値と同じファイルに
// 接続先だけを差し込む使い方を想定している。新しく作るときは 0600 にする
// (パスワードを書くことがあるため)。途中で落ちても壊れたファイルを残さないよう、
// 一時ファイルに書いてから rename する。
func writeDotenv(path string, kvs []kv) error {
	var lines []string
	mode := os.FileMode(0o600)
	if data, err := os.ReadFile(path); err == nil {
		if trimmed := strings.TrimRight(string(data), "\n"); trimmed != "" {
			lines = strings.Split(trimmed, "\n")
		}
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	done := map[string]bool{}
	for i, line := range lines {
		for _, p := range kvs {
			if strings.HasPrefix(line, p.key+"=") {
				lines[i] = p.key + "=" + p.value
				done[p.key] = true
			}
		}
	}
	for _, p := range kvs {
		if !done[p.key] {
			lines = append(lines, p.key+"="+p.value)
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".sashiki-env-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// emitEnv は cmdEnv の出力先を振り分ける。write が空なら標準出力(パスワード無し)、
// あればそのファイルに書き、標準出力には書いたキーの名前だけを出す。
func emitEnv(b branchView, prefix, urlVar, db, write string) error {
	withPassword := write != ""
	kvs := []kv{
		{prefix + "HOST", b.Host},
		{prefix + "PORT", strconv.Itoa(b.Port)},
		{prefix + "USER", b.User},
	}
	if urlVar != "" {
		kvs = append(kvs, kv{urlVar, connURL(b, dbNameFor(b, db), withPassword)})
	}
	if write == "" {
		for _, p := range kvs {
			fmt.Printf("%s=%s\n", p.key, p.value)
		}
		return nil
	}
	if err := writeDotenv(write, kvs); err != nil {
		return err
	}
	keys := make([]string, len(kvs))
	for i, p := range kvs {
		keys[i] = p.key
	}
	fmt.Printf("wrote %s to %s\n", strings.Join(keys, ", "), write)
	return nil
}
