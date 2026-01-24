# Ubuntu Server へのデプロイ方法

この Bot を Ubuntu サーバーで稼働させるための手順です。
Windows 環境でコンパイルして転送する方法と、サーバー上で直接ビルドする方法の2通りがあります。

## 方法 A: Windows でクロスコンパイルして転送する (推奨)

Windows 上で Linux 形式の実行ファイルを作成し、それをサーバーにコピーします。

### 1. ビルド (Windows PowerShell)
プロジェクトのディレクトリで以下のコマンドを実行します。
```powershell
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -o discord_bot_linux main.go
```
これで `discord_bot_linux` というファイルが生成されます。

### 2. ファイルの転送
以下のファイルを Ubuntu サーバーに転送します (SCP や FTP などを使用)。
- `discord_bot_linux`
- `.env` (サーバー用トークンなどを設定済みの場合)

### 3. 実行権限の付与と実行
Ubuntu サーバー上で以下のコマンドを実行します。
```bash
chmod +x discord_bot_linux
./discord_bot_linux
```

---

## 方法 B: Ubuntu サーバー上でビルドする

Git 経由でコードを取得し、サーバー上でビルドします (Go のインストールが必要です)。

### 1. 準備
```bash
# Go のインストール (未インストールの場合)
sudo apt update
sudo apt install golang

# リポジトリのクローン
git clone https://github.com/Ayu102938/golang_discord_bot.git
cd golang_discord_bot
```

### 2. ビルド
```bash
go build -o discord_bot_linux main.go
```

### 3. 設定
`.env` ファイルを作成し、トークンを設定します。
```bash
nano .env
# DISCORD_BOT_TOKEN=your_token_here を記述して保存
```

---

## 常駐化 (Systemd の設定)

サーバーからログアウトしても Bot が動き続けるようにする設定です。

### 1. サービスファイルの作成
`/etc/systemd/system/discord-bot.service` を作成します。

```bash
sudo nano /etc/systemd/system/discord-bot.service
```

以下の内容を記述します (パスやユーザー名は環境に合わせて変更してください)。

```ini
[Unit]
Description=Discord Timetable Bot
After=network.target

[Service]
Type=simple
User=ubuntu
WorkingDirectory=/home/ubuntu/golang_discord_bot
ExecStart=/home/ubuntu/golang_discord_bot/discord_bot_linux
Restart=always

[Install]
WantedBy=multi-user.target
```

### 2. 起動と自動起動設定
```bash
# 設定の読み込み
sudo systemctl daemon-reload

# 起動
sudo systemctl start discord-bot

# 状態確認 (Active: active (running) ならOK)
sudo systemctl status discord-bot

# PC再起動時も自動で起動するように設定
sudo systemctl enable discord-bot
```
