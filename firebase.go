package main

import (
    "context"
    "fmt"
    "io"
    "os"
    "path/filepath"
    "strings"
    "time"

    "cloud.google.com/go/storage"
    firebase "firebase.google.com/go/v4"
    "google.golang.org/api/option"
)

// FirebaseApp グローバル Firebase アプリケーション
var firebaseApp *firebase.App
var storageClient *storage.Client
var bucketName string

// InitFirebase Firebase を初期化
func InitFirebase(ctx context.Context, serviceAccountPath, bucket string) error {
    var opt option.ClientOption

    // 【修正箇所】環境変数に鍵のJSONデータが直接設定されているかチェック
    firebaseJSON := os.Getenv("FIREBASE_SERVICE_ACCOUNT_JSON")
    if firebaseJSON != "" {
        // Render環境用：環境変数の文字列データを鍵として読み込む
        opt = option.WithCredentialsJSON([]byte(firebaseJSON))
    } else {
        // ローカル開発用：今まで通りMac内のJSONファイルを読み込む
        opt = option.WithCredentialsFile(serviceAccountPath)
    }

    app, err := firebase.NewApp(ctx, nil, opt)
    if err != nil {
        return fmt.Errorf("firebase.NewApp failed: %v", err)
    }

    // Google Cloud Storage クライアント
    client, err := storage.NewClient(ctx, opt)
    if err != nil {
        return fmt.Errorf("storage.NewClient failed: %v", err)
    }

    firebaseApp = app
    storageClient = client
    bucketName = bucket
    return nil
}

// UploadUserAvatar ユーザーアバター画像を Firebase Storage にアップロード
// 戻り値: 公開可能な URL、エラー
func UploadUserAvatar(ctx context.Context, userID string, fileReader io.ReadCloser, fileName string) (string, error) {
    defer fileReader.Close()

    // ファイルパス: users/{userID}/avatar/{fileName}
    filePath := fmt.Sprintf("users/%s/avatar/%s", userID, fileName)

    // 既存のアバターを削除（オプション）
    _ = cleanupOldAvatars(ctx, userID)

    // アップロード
    wc := storageClient.Bucket(bucketName).Object(filePath).NewWriter(ctx)
    wc.ContentType = getContentType(fileName)

    if _, err := io.Copy(wc, fileReader); err != nil {
        return "", fmt.Errorf("upload copy failed: %v", err)
    }

    if err := wc.Close(); err != nil {
        return "", fmt.Errorf("write close failed: %v", err)
    }

    // 公開 URL を生成
    publicURL := fmt.Sprintf(
        "https://storage.googleapis.com/%s/%s",
        bucketName,
        filePath,
    )

    return publicURL, nil
}

// cleanupOldAvatars 古いアバターを削除
func cleanupOldAvatars(ctx context.Context, userID string) error {
    prefix := fmt.Sprintf("users/%s/avatar/", userID)
    it := storageClient.Bucket(bucketName).Objects(ctx, &storage.Query{Prefix: prefix})

    for {
        attrs, err := it.Next()
        if err == storage.ErrObjectNotExist {
            break
        }
        if err != nil {
            return err
        }

        if err := storageClient.Bucket(bucketName).Object(attrs.Name).Delete(ctx); err != nil {
            continue // エラーは無視してスキップ
        }
    }

    return nil
}

// getContentType ファイル拡張子から Content-Type を推定
func getContentType(fileName string) string {
    ext := strings.ToLower(filepath.Ext(fileName))
    switch ext {
    case ".jpg", ".jpeg":
        return "image/jpeg"
    case ".png":
        return "image/png"
    case ".gif":
        return "image/gif"
    case ".webp":
        return "image/webp"
    default:
        return "image/jpeg"
    }
}

// GetSignedURL 署名付き一時 URL を生成（例: プライベート画像用）
func GetSignedURL(ctx context.Context, filePath string, expiration time.Duration) (string, error) {
    signedURL, err := storageClient.Bucket(bucketName).SignedURL(filePath, &storage.SignedURLOptions{
        Method:  "GET",
        Expires: time.Now().Add(expiration),
    })

    if err != nil {
        return "", fmt.Errorf("signed url failed: %v", err)
    }

    return signedURL, nil
}