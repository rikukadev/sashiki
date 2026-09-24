# ブランチ保護(main)— 運用手順(#14)

private + 無料プランではブランチ保護ルールセットが使えないため、現在は
`.githooks/pre-push` のローカルフックで代替している。**public 化(または Pro/Team
化)したら**、GitHub 側でルールセットを有効化する。これはリポジトリ管理者の運用作業。

## 推奨ルールセット(main)

- **直 push 禁止**(PR 経由のみ)
- **PR 必須**、マージ前に **1 レビュー承認**(単独運用なら 0 でも可)
- **required status checks = `test` / `lint` / `e2e`**(strict: 最新 main で通っていること)。
  `install-sh` / `action-ssm` も軽く決定的なので required にしてよい。`e2e-aws` / `e2e-postgres` /
  `vuln` は advisory(実 AWS・安定化待ち・新規 CVE で無関係な PR を止めないため。基準は SPEC 27 章)
- **force push 禁止 / 削除禁止**
- **会話の解決を必須**(require conversation resolution)
- 任意: **linear history**(squash マージ運用と相性が良い)

## gh CLI で適用する例

`OWNER/REPO` を置き換えて管理者権限のトークンで実行する。

```bash
gh api -X POST repos/rikukadev/sashiki/rulesets \
  -f name='main-protection' \
  -f target='branch' \
  -f enforcement='active' \
  -F 'conditions[ref_name][include][]=~DEFAULT_BRANCH' \
  -F 'rules[][type]=deletion' \
  -F 'rules[][type]=non_fast_forward' \
  -F 'rules[][type]=pull_request' \
  -F 'rules[][type]=required_status_checks' \
  -F 'rules[][parameters][required_status_checks][][context]=test' \
  -F 'rules[][parameters][required_status_checks][][context]=lint' \
  -F 'rules[][parameters][required_status_checks][][context]=e2e' \
  -F 'rules[][parameters][strict_required_status_checks_policy]=true'
```

> 注: ルールセット API のペイロードは配列/入れ子が多く `-F` の組み立てが繊細。
> うまくいかない場合は GitHub UI(Settings → Rules → Rulesets)で上記項目を設定するのが確実。
> チェック名 `test` / `lint` / `e2e` は `.github/workflows/ci.yml` の job 名と一致させること。

## 同時にやること(public 化タイミング)

- `SECURITY.md`(脆弱性報告ポリシー)— 整備済み
- ライセンス表記 — `LICENSE` / `NOTICE` / `THIRD-PARTY-LICENSES.md` 整備済み
- ローカルフック `.githooks/pre-push` は保険として残してよい(ルールセットと二重で無害)
