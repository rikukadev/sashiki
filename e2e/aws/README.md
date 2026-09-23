# 実 AWS の E2E

EC2 を 1 台立てて、利用者と同じ手順で sashiki を入れ、使い終わったら捨てる。

```bash
goreleaser release --snapshot --clean --skip=publish,sign,announce
AWS_REGION=ap-northeast-1 E2E_ARTIFACT_BUCKET=sashiki-e2e-artifacts-apne1-<account> \
  ./e2e/aws/run.sh dist/sashiki_*_linux_amd64.deb my-tag
```

## ここでしか見えないもの

[`e2e/e2e.sh`](../e2e.sh) はループバックの zpool で一通りを見るが、
次の 3 つはそこを通らない。

- **実 EBS デバイスへの `init`。** `/dev/sdb` と指定しても Nitro では
  `/dev/nvme1n1` として見える。`provision.sh` がデバイス名を決め打ちせず
  「マウントされていない空きディスク」を探しているのはそのため
- **deb 経由の導入。** `sashikid.service` / `sashiki` ユーザー /
  `/var/lib`・`/var/log` の用意はパッケージの仕事で、バイナリを直接置くと
  まるごと検証されない
- **Action の `transport=ssm` が端から端まで。**
  [`e2e/action-ssm.sh`](../action-ssm.sh) は偽の `aws` で「送る形」しか見ない。
  実際に SSM が届いてインスタンス上の CLI が動いて出力が返ることは、
  ここでしか確かめられない

**`transport=ssm` の `delete` が一度も成功していなかった**
([#263](https://github.com/rikukadev/sashiki/pull/263))のは、この段が
無かったからで、デモ環境の PR を閉じて初めて見つかった。

## 片付け

生 EC2 は CloudFormation と違って**誰も片付けてくれない**。三重にしてある:

1. `run.sh` の `trap`(正常終了でも失敗でも terminate)
2. workflow の `if: always()` ステップ(スクリプトごと落ちた場合の網)
3. `Name` タグ(`sashiki-e2e-<run_id>`)— 1 も 2 も駄目だったときに探せる

成果物バケットは**ライフサイクルで 1 日**。消し忘れても積み上がらない。

取りこぼしを探す:

```bash
aws ec2 describe-instances \
  --filters "Name=tag:sashiki-e2e,Values=true" "Name=instance-state-name,Values=running" \
  --query 'Reservations[].Instances[].[InstanceId,LaunchTime]' --output text
```

## AWS 側の用意(一度だけ)

| | 何 | 用途 |
|---|---|---|
| ロール | `sashiki-e2e-github-actions` | CI が OIDC で引き受ける。EC2 の起動 / 破棄・SSM・成果物バケット |
| ロール | `sashiki-e2e-instance`(インスタンスプロファイル) | `AmazonSSMManagedInstanceCore` + 成果物バケットの読み取り |
| バケット | `sashiki-e2e-artifacts-apne1-<account>`(`ci-policy.json` と同名) | deb と `provision.sh` の受け渡し。1 日で消える |

**インバウンドは開けない。** SSM はアウトバウンドだけで足りるので、SSH 鍵も
踏み台も要らない。これが `transport=ssm` を選んだ理由でもある。

CI 変数: `AWS_ROLE_ARN` / `AWS_REGION` / `E2E_ARTIFACT_BUCKET`。

ロールのポリシーは [`ci-policy.json`](ci-policy.json) にある。**手元で通っても
CI で落ちる**ことがあるので(手元は admin、CI は絞ったロール)、権限を足したら
必ずここも更新する:

```bash
aws iam put-role-policy --role-name sashiki-e2e-github-actions \
  --policy-name sashiki-e2e --policy-document file://e2e/aws/ci-policy.json
```

実際 `s3:GetBucketLocation` はこれで見つかった。presign がバケットの
リージョンを引くために呼ぶが、手元では admin なので気づけなかった。

## 費用

`t3.medium` を 10 分ほど + EBS 40GB を同じだけ。**1 回あたり $0.02 未満**。
