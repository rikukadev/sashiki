# sashiki branch action

PR の open/reopen/synchronize/close に連動して sashiki の DB ブランチを管理する composite action。

## 使い方

```yaml
# .github/workflows/sashiki.yml
name: sashiki
on:
  pull_request:
    types: [opened, reopened, synchronize, closed]

jobs:
  branch:
    runs-on: [self-hosted, vpc]   # sashikid の API に届くランナー
    steps:
      - uses: rikukadev/sashiki/action@v0.12.0 # x-release-please-version
        with:
          api_url: http://sashiki.internal:8080
          token: ${{ secrets.SASHIKI_API_TOKEN }}
          profile: preview          # lifecycle profile(省略時はサーバー既定)
          on_close: delete          # delete | keep(keep は TTL 回収に任せる)
          comment: "true"           # PR に接続先をコメント(マーカー付きで冪等更新)
```

- ブランチ名は既定で `pr-<PR番号>`(`branch:` で上書き可)
- **冪等**: create は `exist_ok=true`(既存なら 200)、closed の delete は 404 も成功扱い
- `synchronize`(push)でも create を呼ぶだけ(TTL 削除後の復活を兼ねる)。**migration の再適用は自動では行わない** — 利用者が `sashiki recreate` を明示的に選ぶ(v2 仕様 22-1)
- **provenance**: `source` を省略すると `{"type":"github_pr","repository":"<repo>","ref":"<PR番号>"}` を自動生成して渡す(`sashiki show <name> --json` の `source` に残る)。任意の JSON で上書き可
- outputs: `host` / `port` / `user` — 後続 step でプレビュー環境に渡せる(`DB_HOST` / `DB_PORT` / `DB_USER` へ)

## GitHub-hosted runner から使う(transport: ssm、#244)

API は VPC の中にいることが多く、GitHub-hosted runner からは到達できない。その場合は
**`transport: ssm`** を指定すると、API を叩く代わりに **SSM Run Command でインスタンス上の
`sashiki` CLI** を実行する(CLI は loopback の API を叩くのでトークン不要)。

```yaml
jobs:
  branch:
    runs-on: ubuntu-latest          # GitHub-hosted で良い
    permissions:
      id-token: write               # OIDC で AWS ロールを引き受ける
      contents: read
    steps:
      - uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: arn:aws:iam::<account>:role/<gha-role>
          aws-region: ap-northeast-1
      - uses: rikukadev/sashiki/action@v0.12.0 # x-release-please-version
        with:
          transport: ssm
          instance_id: i-0123456789abcdef0
          profile: preview
```

runner 側のロールに必要な最小権限:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow",
      "Action": ["ssm:SendCommand"],
      "Resource": [
        "arn:aws:ec2:*:*:instance/i-0123456789abcdef0",
        "arn:aws:ssm:*::document/AWS-RunShellScript"
      ] },
    { "Effect": "Allow", "Action": ["ssm:GetCommandInvocation"], "Resource": "*" }
  ]
}
```

インスタンス側は Terraform モジュールが `AmazonSSMManagedInstanceCore` を付けているので追加設定は不要。

> `transport: ssm` では新規作成と既存の区別を取らないため、outputs の `created` は `unknown` になる。

## 操作を明示する(action、#244)

PR イベントからではなく**操作を直接指定**したいとき(ラベル駆動など)は `action:` を使う。
指定すると `on_close` より優先される。

```yaml
# 例: "db-reset" ラベルが付いたらブランチを作成時点に戻す
on:
  pull_request:
    types: [labeled]

jobs:
  reset:
    if: github.event.label.name == 'db-reset'
    runs-on: [self-hosted, vpc]
    steps:
      - uses: rikukadev/sashiki/action@v0.12.0 # x-release-please-version
        with:
          api_url: http://sashiki.internal:8080
          token: ${{ secrets.SASHIKI_API_TOKEN }}
          action: reset
```

`create` / `delete` / `reset` を受け付ける。

## close イベントの取りこぼしについて

Actions の closed イベントは取りこぼすことがあるため、削除はこの action だけに依存せず
TTL 自動回収(issue #8)と併用する。
