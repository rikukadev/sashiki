# Security Policy

## 位置づけ

sashiki は**非本番(開発 / CI / プレビュー)向けの、使い捨て DB を配る基盤**です。
本番データベースの置き換えを意図していません。運用上の前提:

- API(sashikid)は **loopback からは無認証**、それ以外は **Bearer トークン**必須
  (ADR-005)。**リバースプロキシ越しに公開すると接続元が 127.0.0.1 に見えて素通し**に
  なるため、外部公開時は sashikid を直接 listen させ、トークン必須で運用してください。
- **本番データを baseline に入れるならマスキング**を build の必須ステップにできます
  (`require_masked`)。未マスクの baseline は publish 不可にできます。
- プロキシ(方式A)は `<user>@<branch>` の user 部と `app_pass` で認証を終端します。
  `proxy.allowed_user` を絞る/秘密は SSM・Secrets Manager から供給する運用を推奨します。
- `proxy.passthrough_users` に列挙した user は例外で、認証をブランチ内の mysqld に**委ねて中継**します
  (sashiki はそのパスワードを持ちません)。この経路は既存ブランチだけを解決し lazy create はしませんが、
  **sleeping ブランチの起床は認証前に起きます**(起床させる相手は、ブランチ名を知っていて proxy に
  到達できる相手に限られ、authlimit の同時試行・失敗遅延が掛かります)。列挙は必要なユーザーだけに。

## 対応バージョン

v0.x(public preview)。セキュリティ修正は最新のマイナー/パッチにのみ入ります。
API / config は未固定で、マイナー版で破壊的変更があり得ます。

## 脆弱性の報告

**公開 issue には書かないでください。** 次のいずれかで非公開に連絡してください:

- GitHub の **Security Advisories**(リポジトリの「Security」→「Report a vulnerability」)
- 上が使えない場合はメンテナ(@rikukaInoue)へ private に連絡

報告には再現手順・影響範囲・可能なら PoC を添えてください。受領後、修正方針と
公表時期を調整します。責任ある開示に感謝します。
