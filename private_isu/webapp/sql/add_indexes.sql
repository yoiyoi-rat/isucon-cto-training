-- インデックス追加
-- 実行方法: mysql -u isuconp -pisuconp isuconp < add_indexes.sql

-- 適用済み: idx_comments_post_id
-- ALTER TABLE comments ADD INDEX idx_comments_post_id (post_id);

-- ALTER TABLE posts ADD INDEX idx_posts_created_at (created_at);
-- ALTER TABLE posts ADD INDEX idx_posts_user_id (user_id);
