-- ローカル検証用: testuser が connpass の参加イベント当日に他の人とすれ違ったデータ。
-- 本番DBには流さないこと。何度流しても重複しない。
--
--   psql -U user -h localhost -d passly -f mock/encounter_seed.sql
--
-- 仮ユーザーは testuser と同じパスワード (password123) でログインできる。
-- encounters.created_at は UTC で保存し、日別APIは JST の日付で集計する。

BEGIN;

-- 共通タグの検証用に testuser にも技術スタックを入れる
UPDATE users SET tech_stack = 'Flutter,Go' WHERE id = 'testuser';

INSERT INTO users (id, name, email, password_hash, one_word, role, tech_stack, affiliation)
SELECT v.id, v.name, v.email, t.password_hash, v.one_word, v.role, v.tech_stack, v.affiliation
FROM (VALUES
    ('dummy_aoi',  'あおい', 'aoi@example.com',  'Flutter大好き',      'モバイル',     'Flutter,Dart',        '愛知工業大学'),
    ('dummy_ren',  'れん',   'ren@example.com',  'Goでサーバー書いてます', 'バックエンド', 'Go,Docker',           '名古屋工業大学'),
    ('dummy_sora', 'そら',   'sora@example.com', 'インフラ勉強中',      'インフラ',     'Python,AWS',          '名古屋大学'),
    ('dummy_hana', 'はな',   'hana@example.com', 'フロントもバックも',   'フルスタック', 'TypeScript,React,Go', '愛知工業大学'),
    ('dummy_kai',  'かい',   'kai@example.com',  'ネイティブアプリ派',   'モバイル',     'Swift,Kotlin',        '中京大学')
) AS v(id, name, email, one_word, role, tech_stack, affiliation)
CROSS JOIN (SELECT password_hash FROM users WHERE id = 'testuser') t
ON CONFLICT DO NOTHING;

INSERT INTO encounters (my_id, target_id, is_confirmed, created_at)
VALUES
    -- SysHack 2026 (2026-03-31 JST 10:00-20:00): 4人、うち共通タグありは あおい・れん・はな
    ('testuser', 'dummy_aoi',  TRUE, '2026-03-31 02:00:00'),
    ('testuser', 'dummy_ren',  TRUE, '2026-03-31 04:30:00'),
    ('dummy_sora', 'testuser', TRUE, '2026-03-31 06:00:00'),
    ('testuser', 'dummy_hana', TRUE, '2026-03-31 08:15:00'),
    -- MatsuribaMAX 2026 (2026-03-08 JST 10:00-21:00): 2人、れんとは再会
    ('testuser', 'dummy_ren',  TRUE, '2026-03-08 03:00:00'),
    ('dummy_kai', 'testuser',  TRUE, '2026-03-08 05:00:00'),
    -- 29Tech vol.2 (2026-07-17 JST 19:00-20:30): 1人、共通タグなし
    ('testuser', 'dummy_sora', TRUE, '2026-07-17 10:30:00')
    -- 他の参加イベントは0人のまま(空表示の確認用)
ON CONFLICT DO NOTHING;

COMMIT;

-- 片付ける場合:
-- DELETE FROM encounters WHERE my_id LIKE 'dummy_%' OR target_id LIKE 'dummy_%';
-- DELETE FROM users WHERE id LIKE 'dummy_%';
