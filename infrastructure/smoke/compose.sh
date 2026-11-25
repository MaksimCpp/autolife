#!/bin/sh
set -eu

: "${AUTOLIFE_S3_ACCESS_KEY:?export the local S3 access key}"
: "${AUTOLIFE_S3_SECRET_KEY:?export the local S3 secret key}"
endpoint=http://127.0.0.1:8333
api=http://127.0.0.1:8080
bucket=autolife
key="smoke/$(openssl rand -hex 16)"
temp=$(mktemp -d)
AWS_ACCESS_KEY_ID=$AUTOLIFE_S3_ACCESS_KEY
AWS_SECRET_ACCESS_KEY=$AUTOLIFE_S3_SECRET_KEY
AWS_DEFAULT_REGION=us-east-1
AWS_PAGER=
export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_DEFAULT_REGION AWS_PAGER

s3() { aws --endpoint-url "$endpoint" "$@"; }
cleanup() {
  s3 s3api delete-object --bucket "$bucket" --key "$key" >/dev/null 2>&1 || true
  rm -rf -- "$temp"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

curl -fsS "$api/en" | grep -q 'lang="en"'
curl -fsS "$api/ru" | grep -q 'lang="ru"'
curl -fsS "$api/health/live" | grep -q '"status":"ok"'
curl -fsS "$api/health/ready" | grep -q '"status":"ready"'
docker compose run --rm --no-deps migrate
docker compose run --rm --no-deps river-smoke

printf 'AutoLife local integration fixture\n' > "$temp/input"
checksum=$(openssl dgst -sha256 -binary "$temp/input" | openssl base64 -A)
s3 s3api put-object --bucket "$bucket" --key "$key" --body "$temp/input" \
  --checksum-sha256 "$checksum" > /dev/null
test "$(curl -sS -o /dev/null -w '%{http_code}' "$endpoint/$bucket/$key")" = 403
url=$(s3 s3 presign "s3://$bucket/$key" --expires-in 5)
curl -fsS "$url" > "$temp/output"
cmp "$temp/input" "$temp/output"
sleep 6
test "$(curl -sS -o /dev/null -w '%{http_code}' "$url")" = 403
s3 s3api delete-object --bucket "$bucket" --key "$key" > /dev/null
if s3 s3api head-object --bucket "$bucket" --key "$key" >/dev/null 2>&1; then
  echo "S3 object still exists after deletion" >&2
  exit 1
fi

python3 - <<'PY'
from email.message import EmailMessage
from urllib.request import urlopen
import smtplib
import uuid

message = EmailMessage()
message["From"] = "autolife@example.test"
message["To"] = "smoke@example.test"
subject = f"AutoLife smoke {uuid.uuid4()}"
message["Subject"] = subject
message.set_content("Local email integration fixture.")
with smtplib.SMTP("127.0.0.1", 1025, timeout=5) as smtp:
    smtp.send_message(message)
with urlopen("http://127.0.0.1:8025/api/v1/messages", timeout=5) as result:
    assert subject in result.read().decode()
PY

docker compose stop postgres
status=$(curl -sS -o /dev/null -w '%{http_code}' "$api/health/ready") || status=000
docker compose start postgres
test "$status" = 503
docker compose up --wait --wait-timeout 90
