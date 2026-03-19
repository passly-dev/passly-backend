# 今回: 本番環境デプロイと動作確認

## 実施内容
1. ローカルDB接続エラーの解消
   - 状況: `connection refused` エラーが発生。
   - 解決: DockerのPostgreSQLコンテナが停止していたため、`docker-compose up -d` を実行して起動。
2. コードの本番環境 (Render) 対応
   - `main.go` を修正し、`os.Getenv` を用いて `DATABASE_URL` と `PORT` を環境変数から動的に読み込むよう変更。
3. Renderへのデプロイ作業
   - Render上でPostgreSQLとWeb Serviceを作成し、GitHubリポジトリと連携。
   - 環境変数にDBの Internal Database URL を設定。
4. デプロイエラーの解消
   - 状況: `go: go.mod requires go >= 1.26.1 (running go 1.26.0)` のバージョン不一致エラー。
   - 解決: ローカルの `go.mod` を `go 1.26.0` にダウングレードして再プッシュ。
5. 通信テスト
   - `curl -X POST` コマンドで本番URLへテスト送信を実行。
   - 未実装のパスに対し `404 Not Found` や `405 Method Not Allowed` が正しく返ることを確認（サーバーとDBの正常稼働を証明）。

## Next Action
- Flutterアプリからすれ違いデータ（ID、緯度、経度）を受信してDBに保存するPOSTエンドポイントを実装する。