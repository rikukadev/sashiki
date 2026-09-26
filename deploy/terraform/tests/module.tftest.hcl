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
}
