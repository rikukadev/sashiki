# terraform test(mock provider。AWS 資格情報も実リソースも要らない)。
#   cd deploy/terraform && terraform init -backend=false && terraform test
# CI の terraform job が同じものを回す。plan だけで確かめられる不変条件を固定する。

mock_provider "aws" {
  # policy document の json はモックが作れない(ランダム文字列になり aws_iam_role の
  # 検証で落ちる)ので、中身のある JSON を固定で返す。
  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }
}
mock_provider "random" {}

variables {
  name           = "t"
  vpc_id         = "vpc-0123456789abcdef0"
  subnet_ids     = ["subnet-0123456789abcdef0"]
  allowed_sg_ids = ["sg-0123456789abcdef0"]
}

run "defaults" {
  command = apply

  # #341: データ EBS は既定で暗号化。
  assert {
    condition     = aws_ebs_volume.data.encrypted == true
    error_message = "データ EBS が既定で暗号化されていない(#341)"
  }

  # #342: user-data(cloud-init ログ・EC2 属性に残る)に秘密を埋め込まない。
  # dev パスワードと API トークンは instance role で実行時に取得する。
  assert {
    condition     = !strcontains(aws_instance.this.user_data, random_password.dev.result)
    error_message = "user-data に dev パスワードの平文が入っている"
  }
  assert {
    condition     = !strcontains(aws_instance.this.user_data, "Authorization: Bearer")
    error_message = "user-data が Bearer トークンを平文で送っている(#342 の github_token 経路)"
  }
  assert {
    condition     = !strcontains(aws_instance.this.user_data, "github_token") && !strcontains(aws_instance.this.user_data, "GITHUB_TOKEN")
    error_message = "user-data に github_token 経路が残っている(#342)"
  }

  # #340: CI 用トークンは branches scope で state.db に発行し、Terraform は値を持たない。
  # admin トークンは既定で発行しない。
  assert {
    condition     = strcontains(aws_instance.this.user_data, "sashiki token create --name ci --scope branches")
    error_message = "user-data が branches scope のトークンを発行していない(#340)"
  }
  assert {
    condition     = aws_ssm_parameter.api_token.value == "pending-bootstrap"
    error_message = "api_token パラメータの値を Terraform が管理してしまっている(#340)"
  }
  assert {
    condition     = !strcontains(aws_instance.this.user_data, "admin-token") && output.admin_token_ssm_path == null && length(aws_ssm_parameter.admin_token) == 0
    error_message = "admin トークンが既定で発行されている(#340)"
  }
}

# admin トークンは opt-in。
run "admin_token_opt_in" {
  command = apply

  variables {
    admin_token = true
  }

  assert {
    condition     = length(aws_ssm_parameter.admin_token) == 1 && output.admin_token_ssm_path == "/t/sashiki/admin-token"
    error_message = "admin_token = true で admin トークンの置き場が作られない"
  }
  assert {
    condition     = strcontains(aws_instance.this.user_data, "/t/sashiki/admin-token") && !strcontains(aws_instance.this.user_data, random_password.admin_token[0].result)
    error_message = "admin トークンは実行時に SSM から取るべきで、user-data に平文を埋めない"
  }
}

# 既存の非暗号化 volume を持つ環境向けの opt-out(移行までの間だけ)。
run "unencrypted_opt_out" {
  command = apply

  variables {
    data_volume_encrypted = false
  }

  assert {
    condition     = aws_ebs_volume.data.encrypted == false
    error_message = "data_volume_encrypted=false が効いていない"
  }
}

# 利用者管理の KMS key を渡せる。
run "customer_kms_key" {
  command = apply

  variables {
    kms_key_id = "arn:aws:kms:ap-northeast-1:123456789012:key/00000000-0000-0000-0000-000000000000"
  }

  assert {
    condition     = aws_ebs_volume.data.encrypted == true && aws_ebs_volume.data.kms_key_id == var.kms_key_id
    error_message = "kms_key_id がデータ EBS に渡っていない"
  }
}

