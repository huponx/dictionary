-- Removes the seed user; the words, review state, log and category go with it (foreign keys cascade).
DELETE FROM identity_users WHERE id = '00000000-0000-0000-0000-0000000000aa';
