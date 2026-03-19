# すれ違い通信バックエンド (Streetpass Backend)

## 概要
すれ違い通信を実現するためのバックエンドAPIサーバー。
ユーザーの位置情報やIDを受信し、データベースに記録する。

## 技術スタック
- 言語: Go 1.26.0
- フレームワーク: Echo v4
- データベース: PostgreSQL 16
- インフラ: Render (Web Service & PostgreSQL)
- ローカル環境: Docker (Docker Compose)

## ローカル開発環境の立ち上げ手順

1. **データベースの起動**
   Docker Desktopが起動していることを確認し、以下のコマンドを実行。
   ```bash
   docker-compose up -d