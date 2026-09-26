# sashiki を「1 apply」で立てるモジュール(仕様17章)。
# EC2 + データ EBS(prevent_destroy)+ SG + IAM + Route53 + Secrets Manager
# (dev パスワード)+ SSM(API トークン)。user-data で install.sh →
# sashiki init --yes まで走らせ、apply 完了時点で sashikid が稼働している。

data "aws_ami" "ubuntu" {
  count       = var.ami_id == "" ? 1 : 0
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }
  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

locals {
  ami_id = var.ami_id != "" ? var.ami_id : data.aws_ami.ubuntu[0].id
  # endpoint: Route53 を作るならその FQDN、無ければ private IP。
  endpoint = var.route53_zone_id != "" && var.dns_name != "" ? var.dns_name : aws_instance.this.private_ip
  tags     = merge(var.tags, { "app" = "sashiki", "Name" = var.name })
  # sashiki_ref: 明示指定が無ければモジュール同梱の VERSION(= その module ref の
  # タグ)を使う。これで利用側は source の ?ref= を固定するだけでよく、ref の二重
  # 指定(#193)が消える(#199)。VERSION は release-please が更新するため
  # `0.5.0 # x-release-please-version` の形。先頭トークンだけ取り出し `v` を前置する。
  sashiki_ref = var.sashiki_ref != "" ? var.sashiki_ref : "v${trimspace(split(" ", file("${path.module}/VERSION"))[0])}"
  # user-data(新規/置換)と SSM Association(既存環境の移行)で同じ冪等処理を使う。
  persist_state_script = file("${path.module}/persist-state.sh")
}

# --- secrets: dev パスワード(Secrets Manager)/ API トークン(SSM SecureString) ---

resource "random_password" "dev" {
  length  = 24
  special = false
}

# admin トークンは opt-in(#340)。既定では発行しない。
resource "random_password" "admin_token" {
  count   = var.admin_token ? 1 : 0
  length  = 40
  special = false
}

resource "aws_secretsmanager_secret" "dev_password" {
  name = "${var.name}/sashiki/dev-password"
  tags = local.tags
}

resource "aws_secretsmanager_secret_version" "dev_password" {
  secret_id     = aws_secretsmanager_secret.dev_password.id
  secret_string = random_password.dev.result
}

# CI / Action に配る API トークン(#340)。値は user-data が `sashiki token create --scope branches`
# で state.db に発行した平文を put-parameter で入れる(list / revoke / rotate できる。
# state.db は data EBS に永続化されるので compute replacement でも失わない)。Terraform は
# 置き場所だけを作り、値は管理しない(ignore_changes)。
resource "aws_ssm_parameter" "api_token" {
  name        = "/${var.name}/sashiki/api-token"
  description = "sashiki API token (branches scope, issued by the instance at bootstrap)"
  type        = "SecureString"
  value       = "pending-bootstrap"
  tags        = local.tags

  lifecycle {
    ignore_changes = [value]
  }
}

# 運用者向けの admin トークン(opt-in)。SASHIKI_API_TOKEN として sashikid の環境に渡す。
resource "aws_ssm_parameter" "admin_token" {
  count = var.admin_token ? 1 : 0
  name  = "/${var.name}/sashiki/admin-token"
  type  = "SecureString"
  value = random_password.admin_token[0].result
  tags  = local.tags
}

# --- security group: DB(3306)と API(8080)を allowed_sg_ids からのみ許可 ---

resource "aws_security_group" "this" {
  name        = "${var.name}-sashiki"
  description = "sashiki: MySQL branches (3306) + API (8080)"
  vpc_id      = var.vpc_id
  tags        = local.tags

  egress {
    description = "all outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group_rule" "mysql" {
  count                    = length(var.allowed_sg_ids)
  type                     = "ingress"
  from_port                = 3306
  to_port                  = 3306
  protocol                 = "tcp"
  security_group_id        = aws_security_group.this.id
  source_security_group_id = var.allowed_sg_ids[count.index]
  description              = "MySQL from allowed sg"
}

resource "aws_security_group_rule" "api" {
  count                    = length(var.allowed_sg_ids)
  type                     = "ingress"
  from_port                = 8080
  to_port                  = 8080
  protocol                 = "tcp"
  security_group_id        = aws_security_group.this.id
  source_security_group_id = var.allowed_sg_ids[count.index]
  description              = "sashiki API from allowed sg"
}

# --- IAM: instance が secret / ssm を読めるだけの最小権限 ---

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

data "aws_iam_policy_document" "secrets" {
  statement {
    sid       = "ReadDevPassword"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.dev_password.arn]
  }
  # bootstrap で発行した branches トークンを置く(読む必要はない)。
  statement {
    sid       = "WriteApiToken"
    actions   = ["ssm:PutParameter"]
    resources = [aws_ssm_parameter.api_token.arn]
  }
  dynamic "statement" {
    for_each = var.admin_token ? [1] : []
    content {
      sid       = "ReadAdminToken"
      actions   = ["ssm:GetParameter"]
      resources = [aws_ssm_parameter.admin_token[0].arn]
    }
  }
}

resource "aws_iam_role" "this" {
  name               = "${var.name}-sashiki"
  assume_role_policy = data.aws_iam_policy_document.assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy" "secrets" {
  name   = "read-secrets"
  role   = aws_iam_role.this.id
  policy = data.aws_iam_policy_document.secrets.json
}

# SSM 経由の運用(SSH レス)を可能にする。
resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.this.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_instance_profile" "this" {
  name = "${var.name}-sashiki"
  role = aws_iam_role.this.name
  # instance profile にはモジュール側でタグを付けない(#160)。iam:TagInstanceProfile が
  # 要るのはここだけで、PowerUser 相当では弾かれるため。provider の default_tags を
  # 使っている場合はそちらの適用有無に従う(モジュールからは制御できない)。
}

# --- Route53 A レコード(route53_zone_id と dns_name の両方が指定されたとき、#161)---
# endpoint output は dns_name を返すので、その名前を private IP に解決する A レコードを作る。
resource "aws_route53_record" "this" {
  count   = var.route53_zone_id != "" && var.dns_name != "" ? 1 : 0
  zone_id = var.route53_zone_id
  name    = var.dns_name
  type    = "A"
  ttl     = 60
  records = [aws_instance.this.private_ip]
}

# --- データ EBS: prevent_destroy でブランチデータを守る ---

resource "aws_ebs_volume" "data" {
  availability_zone = data.aws_subnet.selected.availability_zone
  size              = var.allocated_storage
  type              = var.ebs_type
  # baseline・全ブランチ・state.db が載る volume なので既定で暗号化する(#341)。
  # encrypted の変更は volume の作り直しになるが prevent_destroy が止める。
  # 既存の非暗号化 volume は data_volume_encrypted=false で維持するか、UPGRADING の
  # 手順(snapshot → 暗号化コピー → state の差し替え)で移行する。
  encrypted  = var.data_volume_encrypted
  kms_key_id = var.kms_key_id != "" ? var.kms_key_id : null
  tags       = merge(local.tags, { "Name" = "${var.name}-data" })

  lifecycle {
    prevent_destroy = true
  }
}

data "aws_subnet" "selected" {
  id = var.subnet_ids[0]
}

resource "aws_volume_attachment" "data" {
  device_name = var.data_device_name
  volume_id   = aws_ebs_volume.data.id
  instance_id = aws_instance.this.id

  # データを消さずにインスタンスだけ入れ替えられるように force detach はしない。
  stop_instance_before_detaching = true
}

# --- EC2: user-data で install.sh → sashiki init --yes まで ---

resource "aws_instance" "this" {
  ami                    = local.ami_id
  instance_type          = var.instance_class
  subnet_id              = var.subnet_ids[0]
  vpc_security_group_ids = [aws_security_group.this.id]
  iam_instance_profile   = aws_iam_instance_profile.this.name
  key_name               = var.key_name != "" ? var.key_name : null
  tags                   = local.tags

  # root ボリューム。既定 8GB(AMI 既定)ではダンプ作業や apt で溢れるため広げる(#162)。
  # ブランチデータ本体は別の data EBS(/tank)に置く。
  root_block_device {
    volume_size = var.root_volume_size
    volume_type = "gp3"
    encrypted   = true
  }

  user_data = templatefile("${path.module}/user-data.sh.tftpl", {
    data_device          = var.data_device_name
    data_volume_id       = aws_ebs_volume.data.id
    pool                 = "tank"
    proxy_user           = var.proxy_user
    sashiki_ref          = local.sashiki_ref
    dev_secret_arn       = aws_secretsmanager_secret.dev_password.arn
    api_token_ssm_path   = aws_ssm_parameter.api_token.name
    admin_token_ssm_path = var.admin_token ? aws_ssm_parameter.admin_token[0].name : ""
    persist_state_script = local.persist_state_script
  })

  # user-data と device 名が変わってもデータ EBS は作り直さない。
  lifecycle {
    ignore_changes = [ami]
  }
}

# user-data は EC2 の起動完了を意味しない。Association を apply の完了条件にして、
# cloud-init と sashikid の health endpoint が成功するまで待つ。同じスクリプトを
# 冪等実行するため、既存 module の更新時には root volume 上の state.db もここで
# データ EBS へ移行される。
resource "aws_ssm_association" "bootstrap_ready" {
  name             = "AWS-RunShellScript"
  association_name = "${var.name}-sashiki-bootstrap-ready"

  targets {
    key    = "InstanceIds"
    values = [aws_instance.this.id]
  }

  parameters = {
    commands = join("\n", [
      "set -eu",
      "cloud-init status --wait",
      "printf '%s' '${base64encode(local.persist_state_script)}' | base64 --decode > /tmp/sashiki-persist-state",
      "chmod 0700 /tmp/sashiki-persist-state",
      "SASHIKI_POOL=tank /tmp/sashiki-persist-state",
      "for i in $(seq 1 60); do curl -fsS http://127.0.0.1:8080/v1/healthz >/dev/null && exit 0; sleep 5; done",
      "echo 'sashiki: bootstrap 後も health check が成功しません' >&2",
      "journalctl -u sashikid --no-pager -n 50 >&2 || true",
      "exit 1",
    ])
  }

  wait_for_success_timeout_seconds = 1800

  depends_on = [
    aws_volume_attachment.data,
    aws_iam_role_policy_attachment.ssm,
  ]
}
