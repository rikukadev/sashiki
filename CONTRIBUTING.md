# Contributing

## 開発フロー

1. Issue を立てる(バグ報告・提案どちらも歓迎)
2. ブランチを切って PR を出す。1 PR に修正と改善を混ぜない
3. `make test` が通ること。ロジック変更にはテストを付ける
4. 実装中に下した設計判断は `docs/DECISIONS.md` に ADR として追記する

## テスト

- `make test` — ユニット(ZFS 不要、モック)
- `make e2e-local` — macOS から Lima VM で実 ZFS + mysqld の E2E
- `sudo ./e2e/e2e.sh bin/sashikid bin/sashiki` — Ubuntu ホスト上で直接

## 不変条件(壊さないこと)

- **@init / @baseline スナップショットは必ず mysqld の正常終了状態でのみ取得する**
- storage / engine の実装依存をコア(internal/workspace)に持ち込まない
- 自社固有処理はコアに入れず hooks に置く

## ブランチ運用

- **main への直 push 禁止**(セルフ開発でも)。`work/<topic>` ブランチ → PR → CI green → マージ
- ローカルでは `git config core.hooksPath .githooks` で pre-push フックが直 push をブロックする(クローン後に 1 回実行)
- リポジトリを public 化したら GitHub のルールセットでも main を保護する(PR 必須 + test/lint/e2e 必須)
