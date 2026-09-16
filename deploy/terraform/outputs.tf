# 出力は Aurora / RDS モジュールと揃える。ラッパー(engine 切替)が同じ出力名で
# 参照できるようにする。branch_user は sashiki 固有(Aurora 側は "" を返す互換トリック)。

output "endpoint" {
  description = "接続先(Route53 指定時は FQDN、無ければ private IP)"
  value       = local.endpoint
}

output "reader_endpoint" {
  description = "リーダーエンドポイント。単一ノードのため endpoint と同一(RDS 互換)"
  value       = local.endpoint
}

output "port" {
  description = "MySQL ポート"
  value       = 3306
}

output "username" {
  description = "接続ユーザー名(プロキシユーザー)"
  value       = var.proxy_user
}

output "password_secret_arn" {
  description = "dev パスワードを保存した Secrets Manager の ARN"
  value       = aws_secretsmanager_secret.dev_password.arn
}

output "security_group_id" {
  description = "sashiki の security group"
  value       = aws_security_group.this.id
}

output "branch_user" {
  description = "ブランチ接続の実ユーザー(接続は <user>@<branch>)。RDS/Aurora では \"\" を返す互換出力"
  value       = var.proxy_user
}

output "api_url" {
  description = "sashiki API の URL(create/delete/drain 等)。http のみ(TLS 終端は持たない)。allowed_sg_ids からのみ到達でき、Bearer トークン(api_token_ssm_path)が要る"
  value       = "http://${local.endpoint}:8080"
}

output "api_token_ssm_path" {
  description = "API トークン(SecureString)を保存した SSM パラメータ名"
  value       = aws_ssm_parameter.api_token.name
}

output "instance_id" {
  description = "EC2 インスタンス ID(SSM ポートフォワードで Web UI / API を使うときに指定)"
  value       = aws_instance.this.id
}
