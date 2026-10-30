-- LAM-172: status.color and label.color hold a hue NAME, not a hex.
--
-- The design system has eight hues (slate, blue, teal, green, amber, clay, rust,
-- violet) and the client picks the shade from its own tokens, light or dark. Until now
-- the pickers stored Tailwind hexes (#3b82f6 ...) and the frontend mapped each back to
-- a hue for display. This converts what is already stored, using the SAME mapping as
-- the frontend's statusLook.hueFor and the Go internal/hue package, so no chip changes
-- colour: whatever a row looked like before, it looks like now.
--
-- What each stored value becomes:
--   a hue name, in any case or with spaces   -> that name, lower case
--   one of the sixteen known hexes           -> its hue (the eight Tailwind values the
--                                               old pickers wrote, and the vault's)
--   any other #rgb or #rrggbb hex            -> the nearest hue on the colour wheel;
--                                               a grey (saturation under 0.25) is slate
--   anything else ("red", "", a typo)        -> slate. This is the fallback: it is what
--                                               the frontend already drew for a value it
--                                               could not read, so nothing visibly moves,
--                                               and slate is the neutral hue, the one
--                                               that claims nothing about the work.
--
-- Safe on a database with hand-entered or API-written colours: it never fails on a
-- value, never touches a row already holding a hue name, and is idempotent (a second
-- run changes nothing). The Down cannot recover a hex a team picked outside the eight,
-- because that was already lost here; it gives each hue name back the Tailwind hex the
-- old pickers stored, which the old frontend maps to the same hue.
--
-- The CHECK is deliberately not added. The application normalises on write
-- (internal/hue); a CHECK would make a rollback to older code that still writes hexes
-- fail every status and label write, rather than store a hex the old frontend can read.

-- +migrate Up

CREATE FUNCTION lf_hue_for(c text) RETURNS text LANGUAGE plpgsql IMMUTABLE AS $fn$
DECLARE
    h       text := lower(regexp_replace(c, '^\s+|\s+$', '', 'g'));
    v       integer;
    r       double precision;
    g       double precision;
    b       double precision;
    mx      double precision;
    mn      double precision;
    spread  double precision;
    light   double precision;
    sat     double precision;
    ang     double precision;
    names   text[]             := ARRAY['rust', 'clay', 'amber', 'green', 'teal', 'blue', 'violet', 'rust'];
    angles  double precision[] := ARRAY[3, 23, 41, 142, 186, 210, 265, 363];
    best    integer := 1;
    i       integer;
BEGIN
    IF h IN ('slate', 'blue', 'teal', 'green', 'amber', 'clay', 'rust', 'violet') THEN
        RETURN h;
    END IF;
    IF h ~ '^#[0-9a-f]{3}$' THEN
        h := '#' || substr(h, 2, 1) || substr(h, 2, 1) || substr(h, 3, 1) || substr(h, 3, 1)
                 || substr(h, 4, 1) || substr(h, 4, 1);
    END IF;
    IF h !~ '^#?[0-9a-f]{6}$' THEN
        RETURN 'slate';
    END IF;
    h := replace(h, '#', '');

    CASE h
        WHEN '94a3b8', '6b757d' THEN RETURN 'slate';
        WHEN '3b82f6', '4a7fb5' THEN RETURN 'blue';
        WHEN '14b8a6', '3f9ea8' THEN RETURN 'teal';
        WHEN '22c55e', '4e9a6a' THEN RETURN 'green';
        WHEN 'f59e0b', 'a8873f' THEN RETURN 'amber';
        WHEN 'c2410c', 'b0724a' THEN RETURN 'clay';
        WHEN 'ef4444', 'b05a55' THEN RETURN 'rust';
        WHEN '8b5cf6', '8a6fb0' THEN RETURN 'violet';
        ELSE NULL;
    END CASE;

    v := ('x' || h)::bit(24)::integer;
    r := ((v >> 16) & 255) / 255.0;
    g := ((v >> 8) & 255) / 255.0;
    b := (v & 255) / 255.0;

    mx := greatest(r, g, b);
    mn := least(r, g, b);
    spread := mx - mn;
    light := (mx + mn) / 2;
    IF spread = 0 THEN
        RETURN 'slate';
    END IF;
    sat := spread / (1 - abs(2 * light - 1));
    IF sat < 0.25 THEN
        RETURN 'slate';
    END IF;

    IF mx = r THEN
        ang := (g - b) / spread;
    ELSIF mx = g THEN
        ang := (b - r) / spread + 2;
    ELSE
        ang := (r - g) / spread + 4;
    END IF;
    ang := ang * 60 + 360;
    IF ang >= 360 THEN
        ang := ang - 360;
    END IF;

    FOR i IN 2..8 LOOP
        IF abs(angles[i] - ang) < abs(angles[best] - ang) THEN
            best := i;
        END IF;
    END LOOP;

    RETURN names[best];
END
$fn$;

UPDATE status SET color = lf_hue_for(color)
 WHERE color NOT IN ('slate', 'blue', 'teal', 'green', 'amber', 'clay', 'rust', 'violet');

UPDATE label SET color = lf_hue_for(color)
 WHERE color NOT IN ('slate', 'blue', 'teal', 'green', 'amber', 'clay', 'rust', 'violet');

DROP FUNCTION lf_hue_for(text);

-- +migrate Down

UPDATE status SET color = CASE color
    WHEN 'slate'  THEN '#94a3b8' WHEN 'blue'  THEN '#3b82f6'
    WHEN 'teal'   THEN '#14b8a6' WHEN 'green' THEN '#22c55e'
    WHEN 'amber'  THEN '#f59e0b' WHEN 'clay'  THEN '#c2410c'
    WHEN 'rust'   THEN '#ef4444' WHEN 'violet' THEN '#8b5cf6'
    ELSE color END;

UPDATE label SET color = CASE color
    WHEN 'slate'  THEN '#94a3b8' WHEN 'blue'  THEN '#3b82f6'
    WHEN 'teal'   THEN '#14b8a6' WHEN 'green' THEN '#22c55e'
    WHEN 'amber'  THEN '#f59e0b' WHEN 'clay'  THEN '#c2410c'
    WHEN 'rust'   THEN '#ef4444' WHEN 'violet' THEN '#8b5cf6'
    ELSE color END;
