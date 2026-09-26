# 入力は Aurora / RDS モジュールと揃える(仕様17章)。
# 「RDS を選ぶところで sashiki を選べる」ため、ラッパーモジュールが
# engine=aurora|rds-mysql|sashiki を同じ変数で切り替えられるようにする。

variable "name" {
  description = "リソース名のプレフィックス(RDS の identifier 相当)"
  type        = string
}

variable "vpc_id" {
  description = "配置先 VPC"
  type        = string
}

variable "subnet_ids" {
  description = "配置先サブネット(EC2 は subnet_ids[0] に置く。list を受けるのは RDS モジュール互換のため)"
  type        = list(string)

  validation {
    condition     = length(var.subnet_ids) > 0
    error_message = "subnet_ids には最低1つのサブネットが必要です。"
  }
}

variable "allowed_sg_ids" {
  description = "DB(3306)/ API(8080)への接続を許可する security group の一覧"
  type        = list(string)
  default     = []
}

variable "instance_class" {
  description = "EC2 インスタンスタイプ(RDS の instance_class 相当)。変更前に `sashiki drain` で全ブランチを停止すること"
  type        = string
  default     = "m6i.large"
}

variable "allocated_storage" {
  description = "データ用 EBS のサイズ(GiB)。ZFS プールになる"
  type        = number
  default     = 100
}

variable "root_volume_size" {
  description = "root EBS のサイズ(GiB)。AMI 既定 8GB では apt/ダンプ作業で溢れるため既定 30(#162)。ブランチデータは別の data EBS)"
  type        = number
  default     = 30
}

variable "ami_id" {
  description = "ベース AMI。空なら最新の Ubuntu 24.04 LTS(amd64)を自動解決する"
  type        = string
  default     = ""
}

variable "ebs_type" {
  description = "データ EBS のボリュームタイプ"
  type        = string
  default     = "gp3"
}

variable "data_volume_encrypted" {
  description = "データ EBS(baseline / 全ブランチ / state.db)を暗号化する。既定 true(#341)。既存の非暗号化 volume を持つ環境は、移行するまで false にして prevent_destroy との衝突を避ける(docs/UPGRADING.md)"
  type        = bool
  default     = true
}

variable "kms_key_id" {
  description = "データ EBS の暗号化に使う KMS key の ARN。空なら AWS 管理キー(aws/ebs)"
  type        = string
  default     = ""
}

variable "data_device_name" {
  description = "データ EBS のデバイス名(user-data の zpool 作成に渡す)"
  type        = string
  default     = "/dev/xvdf"
}

variable "route53_zone_id" {
  description = "エンドポイント用の Route53 ホストゾーン ID。空なら DNS レコードを作らず private IP を endpoint に返す"
  type        = string
  default     = ""
}

variable "dns_name" {
  description = "エンドポイントの FQDN(route53_zone_id 指定時に A レコードを作る)。例 db.internal.example.com"
  type        = string
  default     = ""
}

variable "key_name" {
  description = "SSH キーペア名(空なら SSH 鍵を紐付けない)"
  type        = string
  default     = ""
}

variable "proxy_user" {
  description = "ブランチ接続用のプロキシユーザー名(branch_user 出力に使う)"
  type        = string
  default     = "dev"
}

variable "sashiki_ref" {
  # 空(既定)なら module 同梱の deploy/terraform/VERSION(= その module ref の
  # タグ)を使う(#199)。つまり利用側は source の ?ref=vX.Y.Z を固定するだけでよく、
  # バイナリ版はモジュールと自動一致する。明示的に別タグを入れたいときだけ設定する。
  description = "sashiki のリリースタグ。空ならモジュール同梱の VERSION を使う(= source の ?ref= と自動一致、#199)。上書きするなら module の ?ref= と同じ値にすること(#193)"
  type        = string
  default     = ""
}

variable "tags" {
  description = "全リソースに付与するタグ"
  type        = map(string)
  default     = {}
}
