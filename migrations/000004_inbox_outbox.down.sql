DROP TRIGGER IF EXISTS outbox_events_guard_update ON outbox_events;
DROP TABLE IF EXISTS outbox_events;
DROP FUNCTION IF EXISTS outbox_events_guard();
DROP TRIGGER IF EXISTS inbox_messages_guard_update ON inbox_messages;
DROP TABLE IF EXISTS inbox_messages;
DROP FUNCTION IF EXISTS inbox_messages_guard();
