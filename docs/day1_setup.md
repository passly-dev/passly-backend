# 初日: プロジェクト初期化とローカル環境構築

## 実施内容
1. プロジェクトの初期設定
   - `go mod init` によるGoモジュールの初期化。
   - Echoフレームワーク (`github.com/labstack/echo/v4`) とPostgreSQLドライバ (`github.com/lib/pq`) の導入。
2. ローカルデータベース環境の構築
   - `docker-compose.yml` を作成し、ローカル開発用のPostgreSQL環境を定義。
3. サーバーのベース実装
   - `main.go` を作成。
   - DBへの接続処理と、データ保存用の `encounters` テーブルを自動生成するSQLクエリを実装。
4. 環境トラブルシューティング
   - ターミナル再起動時に `go` コマンドが認識されない問題に対し、`~/.zshrc` に環境変数PATHを記述して永続化。