-- Schema version 3: hiding an item is permanent, and a hidden item carries no gofer
-- judgment (worklist.StripScoring). Items hidden before this rule still hold their
-- signals and score: delete them, keeping only the GitHub labels (display data) and
-- the hide and completion marks. A zero time.Time encodes as its zero value, not as
-- an absent field. Nothing else in the document changes: the GitHub fields and the
-- thread stay.
UPDATE work_items
SET doc = json_set(doc,
        '$.signals', json_object('labels', json(coalesce(json_extract(doc, '$.signals.labels'), '[]'))),
        '$.gofer', json_object(
            'hidden_at', json_extract(doc, '$.gofer.hidden_at'),
            'completed_at', coalesce(json_extract(doc, '$.gofer.completed_at'), '0001-01-01T00:00:00Z')))
WHERE coalesce(json_extract(doc, '$.gofer.hidden_at'), '0001-01-01T00:00:00Z') <> '0001-01-01T00:00:00Z';
