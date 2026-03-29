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

## connpass API申請待ち向けモック運用

- `/events` と `/users/:username/events` は、`CONNPASS_MOCK_MODE=true` のときにJSONモックを返します。
- モックファイルのデフォルトパスは `mock/connpass_events.json` です。
- 別パスを使う場合は `CONNPASS_MOCK_FILE=/path/to/file.json` を設定できます。
- サーバー起動時にモックJSONをRender Postgresへupsert保存します（デフォルト有効）。
- 起動時seedを止める場合は `CONNPASS_MOCK_SEED_ON_STARTUP=false` を設定します。
- API申請が通ったら `CONNPASS_MOCK_MODE=false`（または未設定）で実APIを利用できます。

Render Postgres保存テーブル:

- `connpass_mock_events` テーブルへ `event_id`, `title`, `started_at`, `ended_at`, `owner_nickname`, `place` などを保存します。
- `/events` と `/users/:username/events` は、モック利用時にこのテーブルのデータを優先参照します。
- DBにデータがない場合のみ JSON (`CONNPASS_MOCK_FILE`) → 生成モックの順でフォールバックします。

モックJSONの形式:

```json
{
  "results_returned": 1,
  "results_available": 1,
  "results_start": 1,
  "events": [
    {
      "event_id": 12345,
      "title": "サンプル勉強会",
      "catch": "これは仮のイベントデータです",
      "description": "API申請中のテスト用データです。",
      "event_url": "https://connpass.com/event/12345/",
      "started_at": "2026-04-01T19:00:00+09:00",
      "ended_at": "2026-04-01T21:00:00+09:00",
      "limit": 10,
      "hash_tag": "sample",
      "event_type": "participation",
      "accepted": 5,
      "waiting": 0,
      "updated_at": "2026-03-30T10:00:00+09:00",
      "owner_id": 999,
      "owner_nickname": "test_user",
      "owner_display_name": "テストユーザー",
      "place": "オンライン",
      "address": ""
    }
  ]
}
```

## ローカル開発環境の立ち上げ手順

1. **データベースの起動**
   Docker Desktopが起動していることを確認し、以下のコマンドを実行。
   ```bash
   docker-compose up -d

## プログラム解説

このプログラムは、Go言語で書かれたすれ違い通信バックエンドのAPIサーバーです。Echoフレームワークを使用してRESTful APIを提供し、PostgreSQLデータベースでデータを管理します。主な機能は、ユーザーの登録・ログイン、すれ違い（encounter）の記録と取得です。

### プログラムの構造

- **パッケージとインポート**: `package main` でメイン関数を定義。必要なライブラリとして、Echoフレームワーク、PostgreSQLドライバー、bcrypt暗号化ライブラリなどをインポートしています。

- **構造体定義 (Models)**:
  - `User`: ユーザーの情報を表す構造体。ID、名前、メール、パスワード、アイコンURL、一言、役割、技術スタック、SNS URL、所属などを含む。
  - `Event`: イベントの情報を表す構造体。ID、名前、場所、開始・終了時間を含む。
  - `Encounter`: すれ違いの記録を表す構造体。自分のID、相手のID、イベントID、確認フラグ、作成時間を含む。

- **ヘルパー関数**:
  - `respondError`: エラーレスポンスをJSON形式で返す関数。

- **main関数**:
  - データベース接続: 環境変数 `DATABASE_URL` から接続情報を取得。デフォルトはローカルのPostgreSQL。
  - データベース初期化: `initDB` 関数でテーブルを作成。
  - Echoインスタンス作成: ミドルウェア（ロガー、リカバリー、CORS）を適用。
  - エンドポイント定義: 各種APIエンドポイントを設定。

### APIエンドポイント

1. **POST /signup**: ユーザー登録。ID、名前、メール、パスワードを受け取り、ハッシュ化してデータベースに保存。
2. **POST /login**: ログイン。メールとパスワードで認証し、ユーザーIDを返す。
3. **GET /plaza/count**: 広場画面用。総ユーザー数、本日のすれ違い数を返す。
4. **GET /users/:id/home**: ホーム画面用。総すれ違い数、本日のすれ違い数、未確認のすれ違い、ランダム3人のデータを返す。
5. **PUT /users/:id/encounters/confirm**: ホーム画面のポップアップ既読化。未確認のすれ違いを確認済みに更新。
6. **GET /users/:id/calendar/daily**: カレンダー画面用。指定日のすれ違い数とイベント名を返す。
7. **GET /users/:id/encounters**: 広場画面用。ランダム10人のすれ違いデータを返す。
8. **GET /users/:id**: プロフィール画面用。指定ユーザーの詳細情報を返す。
9. **POST /encounters**: BLE通信用。すれ違いを記録。
10. **GET /**: サーバー稼働確認用。

### データベース初期化 (initDB関数)

- `users` テーブル: ユーザー情報。
- `events` テーブル: イベント情報。
- `event_participants` テーブル: イベント参加者。
- `encounters` テーブル: すれ違い記録。

このプログラムは、すれ違い通信アプリのバックエンドとして、ユーザーの位置情報やIDを処理し、データベースに記録・取得する役割を果たします。