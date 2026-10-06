#!/usr/bin/env bash
# 补充登录通道的端到端验证：起一个真实服务，打真实接口。
#
# 覆盖：
#   1) /api/login/browsers      —— 真实枚举本机浏览器配置
#   2) /api/login/import        —— 真实 BBDown.data 走完整解析 + 校验链
#   3) /api/login/password      —— 真实打到电视端登录接口（用不存在的账号）
#
# 用不存在的账号是刻意的：能拿到「用户名或密码错误」就证明 RSA 加密与
# appkey 签名都被服务端接受了 —— 这正是最容易静默出错的一环。
set -u
cd "$(dirname "$0")/.."
source tools/goenv.sh

BIN=_e2e/bgui.exe
mkdir -p _e2e
go build -o "$BIN" ./cmd/bgui || { echo "构建失败"; exit 1; }

echo "== 启动服务（不自动开浏览器） =="
./"$BIN" -no-open >_e2e/server.log 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null; wait $SRV 2>/dev/null' EXIT

# 端口会从 18230 顺延，探到哪个算哪个
PORT=""
for _ in $(seq 1 40); do
  for p in $(seq 18230 18240); do
    if curl -s --noproxy '*' -m 1 "http://127.0.0.1:$p/api/status" 2>/dev/null | grep -q BBDown; then
      PORT=$p; break 2
    fi
  done
  sleep 0.25
done
if [ -z "$PORT" ]; then echo "没探到服务端口"; cat _e2e/server.log; exit 1; fi
BASE="http://127.0.0.1:$PORT"
echo "服务在 $BASE"
H=(-H "X-BBDown-Native: 1" -H "Content-Type: application/json")

hr() { printf '\n---------- %s ----------\n' "$1"; }

hr "1) GET /api/login/browsers"
curl -s --noproxy '*' -m 20 "$BASE/api/login/browsers"

hr "2) POST /api/login/import —— 真实 BBDown.data"
if [ ! -f ../BBDown.data ]; then
  echo "(没找到 BBDown.data，跳过)"
else
  # 交给 node 拼 JSON，免得 cookie 里的特殊字符把请求体弄坏
  BODY=$(node -e '
    const fs=require("fs");
    process.stdout.write(JSON.stringify({text: fs.readFileSync(process.argv[1],"utf8")}));
  ' ../BBDown.data)
  curl -s --noproxy '*' -m 30 "${H[@]}" -X POST "$BASE/api/login/import" --data "$BODY"
fi

hr "3) POST /api/login/import —— 一段无关文本（应当被拒）"
curl -s --noproxy '*' -m 20 "${H[@]}" -X POST "$BASE/api/login/import" \
  --data '{"text":"这段文字里没有任何凭据信息"}'

hr "4) POST /api/login/password —— 不存在的账号（验证签名与加密是否被接受）"
curl -s --noproxy '*' -m 40 "${H[@]}" -X POST "$BASE/api/login/password" \
  --data '{"username":"zzz_not_exist_9x7","password":"not-a-real-password"}'

hr "5) POST /api/login/from-browser —— 取第一个配置试导入"
IDX=$(curl -s --noproxy '*' -m 20 "$BASE/api/login/browsers" \
  | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{const j=JSON.parse(s);process.stdout.write(j.profiles&&j.profiles.length?String(j.profiles[0].index):"")}catch(e){}})')
if [ -n "$IDX" ]; then
  curl -s --noproxy '*' -m 40 "${H[@]}" -X POST "$BASE/api/login/from-browser" --data "{\"index\":$IDX}"
else
  echo "(没有可用配置，跳过)"
fi

hr "服务端日志"
cat _e2e/server.log
echo
echo "== 结束 =="
