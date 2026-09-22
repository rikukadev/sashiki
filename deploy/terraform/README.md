# sashiki Terraform モジュール(database module contract)

「RDS を選ぶところで sashiki を選べる」ことをゴールにした、1 apply で完結する
モジュール。EC2 + データ EBS(`prevent_destroy`)+ SG + IAM + Route53 +
Secrets Manager(dev パスワード)+ SSM(API トークン)を作り、user-data で
`install.sh` → `sashiki init --yes` まで走らせる。**apply 完了時点で sashikid が
稼働**している(baseline import はダンプが要るため運用者の次手順)。

## 使い方

```hcl
module "db" {
  source = "github.com/rikukadev/sashiki//deploy/terraform?ref=v0.2.0"

  name           = "myapp-preview"
  vpc_id         = var.vpc_id
  subnet_ids     = var.private_subnet_ids
  allowed_sg_ids = [aws_security_group.app.id]

  instance_class    = "m6i.large"
  allocated_storage = 100

  route53_zone_id = var.internal_zone_id
  dns_name        = "db.internal.example.com"

  # public 化前は private リポジトリ取得用の token が要る
  github_token = var.github_token
  sashiki_ref  = "v0.2.0" # バイナリとモジュールは同じ ref で固定する
}
```

出力(`endpoint` / `port` / `username` / `password_secret_arn` / `api_url` など)を
アプリの env や preview 環境に渡す。

## RDS / Aurora との入れ替え(ラッパーモジュール)

主要な入力・出力名を Aurora / RDS モジュールに寄せているので、`engine` 変数で実体を
切り替える社内ラッパーを書ける。RDS API そのものとの互換性はない。`sashiki` 固有の `branch_user` は、RDS/Aurora 側では
`""` を返す**互換トリック**で吸収する(参照側は常に同じ出力名で書ける)。

```hcl
variable "engine" {
  type    = string
  default = "sashiki" # aurora | rds-mysql | sashiki
}

module "sashiki" {
  count             = var.engine == "sashiki" ? 1 : 0
  source            = "github.com/rikukadev/sashiki//deploy/terraform?ref=v0.2.0"
  name              = var.name
  vpc_id            = var.vpc_id
  subnet_ids        = var.subnet_ids
  allowed_sg_ids    = var.allowed_sg_ids
  instance_class    = var.instance_class
  allocated_storage = var.allocated_storage
}

module "aurora" {
  count  = var.engine == "aurora" ? 1 : 0
  source = "terraform-aws-modules/rds-aurora/aws"
  # name / vpc_id / subnet_ids(→ db_subnet_group)/ instance_class などを同じ変数で渡す
  # ...
}

locals {
  db = var.engine == "sashiki" ? {
    endpoint            = module.sashiki[0].endpoint
    port                = module.sashiki[0].port
    username            = module.sashiki[0].username
    password_secret_arn = module.sashiki[0].password_secret_arn
    branch_user         = module.sashiki[0].branch_user # "dev"
    } : {
    endpoint            = module.aurora[0].cluster_endpoint
    port                = module.aurora[0].cluster_port
    username            = module.aurora[0].cluster_master_username
    password_secret_arn = module.aurora[0].cluster_master_user_secret[0].secret_arn
    branch_user         = "" # Aurora 側は空(互換トリック)
  }
}
```

## instance_class を変える前に: `sashiki drain`

`instance_class` の変更は EC2 の停止 → 起動(または置換)を伴う。ブランチの
mysqld が動いたまま止めると危険なので、**変更前に全ブランチを sleeping にする**:

```sh
# 対象ホストで(SSM 経由でも可)
sashiki drain          # 全 running branch を正常終了 → sleeping(データは残る)
```

`drain` はデータを消さない。再接続 / `sashiki` の Wake で復帰する。
apply 後にサイズが戻ったら通常運用に戻る。

## 入力 / 出力

主要な入力: `name` / `vpc_id` / `subnet_ids` / `allowed_sg_ids` /
`instance_class` / `allocated_storage`。MySQL の版は AMI の apt パッケージで決まり、
版を選ぶ入力は提供しない。
詳細は [`variables.tf`](./variables.tf)。

主要な出力: `endpoint` / `reader_endpoint` / `port` / `username` /
`password_secret_arn` / `security_group_id` / `branch_user` / `api_url` /
`api_token_ssm_path`。詳細は [`outputs.tf`](./outputs.tf)。

## 注意

- データ EBS は `prevent_destroy = true`。`terraform destroy` では消えない
  (故意に消すときは state から外すか lifecycle を一時的に外す)。
- **インスタンスを明示的に差し替えてもデータは残り、init が pool を再 import する**(#246)。
  AMI は `ignore_changes` の対象で、`user_data` の変更も既定では in-place のため、
  それだけでは EC2 を置換しない。`terraform apply -replace` 等で置換した場合もデータ EBS は保持される。
  新インスタンスの `sashiki init` は既存 zpool を検出して `zpool import -f` で
  再利用し(pool が無いときだけ `zpool create`)、ブランチと baseline はそのまま
  使える。`-f` はインスタンス差し替えで hostid が変わるため必要で、EBS は 1 台に
  しか attach されないので他ホストとの同時マウントは起きない。
- API トークンは SSM SecureString(`api_token_ssm_path`)。dev パスワードは
  Secrets Manager(`password_secret_arn`)。どちらも平文で state に近い形で
  持たない運用にすること(`terraform.tfstate` の暗号化・アクセス制限は前提)。
- 単一ノード構成のため `reader_endpoint` は contract の形を揃える目的で `endpoint` と同じ値を返す。
- データ EBS は暗号化を明示せず、AWS Backup / 定期 snapshot も設定しない。
  `prevent_destroy` は復元手段ではないため、必要なら利用側で暗号化とバックアップを設計する。

## API を VPC 内から使う(`api_url` + トークン)

sashikid は `0.0.0.0:8080` で listen し(user-data が init の既定 `127.0.0.1` を
書き換える、#286)、`allowed_sg_ids` の SG からだけ SG で到達できる。VPC 外からは
届かない。`api_url` は **http** で、モジュールは TLS 終端を持たない(VPC 内の
private 通信が前提)。

```bash
export SASHIKI_API_URL=$(terraform output -raw api_url)
export SASHIKI_API_TOKEN=$(aws ssm get-parameter --with-decryption \
  --name "$(terraform output -raw api_token_ssm_path)" --query Parameter.Value --output text)
sashiki list
```

GitHub Action の `transport: api`(既定)はこの経路。runner が VPC 外にいるなら
`transport: ssm` を使う([action/README.md](../../action/README.md))。

## Web UI を手元から使う(SSM ポートフォワード)

Web UI は 401 応答時にトークン入力欄を表示し、同じタブの sessionStorage にだけ保持して
API へ Bearer トークンを送る。SSM ポートフォワードなら SSH 鍵・公開ポートなしでも使える:

```bash
aws ssm start-session --target $(terraform output -raw instance_id) \
  --document-name AWS-StartPortForwardingSession \
  --parameters '{"portNumber":["8080"],"localPortNumber":["8080"]}'
# → ブラウザで http://localhost:8080(ブランチ一覧・データブラウザ)
```

既定では loopback から無認証で全機能が使える。常時公開する場合は
`auth.trust_loopback: false` にし、必要なら ALB + OIDC 等も前段に置くこと。
