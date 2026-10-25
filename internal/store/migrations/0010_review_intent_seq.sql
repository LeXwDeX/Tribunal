-- Deferred review intents. candidate_seq is the high-water mark of allocated
-- review.candidate sequences; barrier_seq is the highest candidate sequence
-- already consumed or invalidated by a run/cancel enqueue. Candidate ingress
-- allocates only candidate_seq; activating it increments generation and moves
-- barrier_seq, so filtering a pending candidate never fences the active review.
ALTER TABLE review_controls ADD COLUMN candidate_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE review_controls ADD COLUMN barrier_seq INTEGER NOT NULL DEFAULT 0;
