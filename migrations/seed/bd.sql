-- =============================================================================
-- Bangladesh Railway development seed. LOCAL USE ONLY. Run after the schema.
-- Idempotent: re-running adds journeys for new dates and changes nothing else.
--
-- Representative intercity services on five corridors from Dhaka (Kamalapur).
-- Train numbers, names, classes and most times follow Wikipedia and seat61
-- (2024–2026); some times, stops, distances and off days are approximations.
-- Check eticket.railway.gov.bd before relying on any of it.
--
-- Rules modelled (spec v4.0):
--   * Classes: S_CHAIR (Shovan Chair), F_SEAT, SNIGDHA (AC chair), AC_S (AC seat),
--     F_BERTH (first class berth), AC_B (AC berth). Confirmed seats only.
--   * One quota, ONLINE. At most 4 seats per user per journey.
--   * Sales open 10 days ahead: 14:00 for eastern-zone trains (Chattogram,
--     Cox's Bazar, Sylhet), 08:00 for western-zone trains (Rajshahi, Khulna).
--     Online sales close 30 minutes before departure.
--   * Coaches carry Bengali-letter codes (KA, KHA, GA, GHA, UMA, CHA, SCHA, JA,
--     JHA, NEO), so seats read "UMA-17".
--   * Fare chart per station pair and class, calibrated to the Dhaka–Chattogram
--     (321 km) fares on seat61: S_CHAIR 450, F_SEAT 685, SNIGDHA 855,
--     F_BERTH 932, AC_S 1025, AC_B 1540 Tk; other pairs scale by distance.
--     Online service charge 20 Tk per ticket.
-- =============================================================================
BEGIN;

INSERT INTO seat_class (class_code, name, is_berth, sort_order) VALUES
  ('AC_B',    'AC Berth',     true,  1),
  ('AC_S',    'AC Seat',      false, 2),
  ('SNIGDHA', 'Snigdha',      false, 3),
  ('F_BERTH', 'First Berth',  true,  4),
  ('F_SEAT',  'First Seat',   false, 5),
  ('S_CHAIR', 'Shovan Chair', false, 6)
ON CONFLICT DO NOTHING;

INSERT INTO station (code, name, name_bn, city) VALUES
  ('DHK', 'Dhaka (Kamalapur)',  'ঢাকা (কমলাপুর)',  'Dhaka'),
  ('BAN', 'Dhaka Biman Bandar', 'ঢাকা বিমানবন্দর', 'Dhaka'),
  ('JOY', 'Joydebpur',          'জয়দেবপুর',        'Gazipur'),
  ('BBR', 'Bhairab Bazar',      'ভৈরব বাজার',      'Bhairab'),
  ('BBA', 'Brahmanbaria',       'ব্রাহ্মণবাড়িয়া',    'Brahmanbaria'),
  ('AKH', 'Akhaura',            'আখাউড়া',          'Akhaura'),
  ('CML', 'Cumilla',            'কুমিল্লা',          'Cumilla'),
  ('FEN', 'Feni',               'ফেনী',            'Feni'),
  ('CTG', 'Chattogram',         'চট্টগ্রাম',         'Chattogram'),
  ('COX', 'Cox''s Bazar',       'কক্সবাজার',       'Cox''s Bazar'),
  ('SHG', 'Shaistaganj',        'শায়েস্তাগঞ্জ',      'Habiganj'),
  ('SRM', 'Sreemangal',         'শ্রীমঙ্গল',         'Moulvibazar'),
  ('KUL', 'Kulaura',            'কুলাউড়া',          'Moulvibazar'),
  ('SYL', 'Sylhet',             'সিলেট',           'Sylhet'),
  ('TNG', 'Tangail',            'টাঙ্গাইল',         'Tangail'),
  ('ISD', 'Ishwardi',           'ঈশ্বরদী',          'Pabna'),
  ('ABD', 'Abdulpur',           'আব্দুলপুর',        'Natore'),
  ('RAJ', 'Rajshahi',           'রাজশাহী',          'Rajshahi'),
  ('BHG', 'Bhanga',             'ভাঙ্গা',           'Faridpur'),
  ('RJB', 'Rajbari',            'রাজবাড়ী',          'Rajbari'),
  ('PRD', 'Poradah',            'পোড়াদহ',          'Kushtia'),
  ('CHD', 'Chuadanga',          'চুয়াডাঙ্গা',        'Chuadanga'),
  ('JSR', 'Jashore',            'যশোর',            'Jashore'),
  ('KHL', 'Khulna',             'খুলনা',           'Khulna')
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------
-- Coach templates. Chair cars: rows along the coach (x), seats across (y) with
-- the aisle at y = 2. Cabin coaches: six cabins, two columns each, a gap
-- column between cabins.
--   S_CHAIR_MG  metre gauge, 2+2, 16 rows = 64      S_CHAIR_BG broad gauge, 2+3, 15 rows = 75
--   SNIGDHA     2+2, 14 rows = 56                   AC_S  6 cabins x 6 seats = 36
--   AC_B / F_BERTH  6 cabins x 4 berths (lower/upper) = 24
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION pg_temp.add_coach(p_layout int, p_pos int, p_coach text, p_kind text) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  base int; cls text; n int := 0; r int; p int; nrows int; per int;
BEGIN
  IF EXISTS (SELECT 1 FROM layout_coach WHERE layout_id = p_layout AND coach_code = p_coach) THEN
    RETURN;
  END IF;
  SELECT COALESCE(max(seat_idx) + 1, 0) INTO base FROM layout_seat WHERE layout_id = p_layout;
  cls := CASE WHEN p_kind LIKE 'S_CHAIR%' THEN 'S_CHAIR' ELSE p_kind END;
  IF p_kind IN ('S_CHAIR_MG', 'S_CHAIR_BG', 'SNIGDHA', 'F_SEAT') THEN
    nrows := CASE p_kind WHEN 'S_CHAIR_MG' THEN 16 WHEN 'S_CHAIR_BG' THEN 15 WHEN 'SNIGDHA' THEN 14 ELSE 12 END;
    per   := CASE p_kind WHEN 'S_CHAIR_BG' THEN 5 ELSE 4 END;
    INSERT INTO layout_coach (layout_id, coach_code, position, class_code, width, height)
    VALUES (p_layout, p_coach, p_pos, cls, nrows, per + 1);
    FOR r IN 0 .. nrows - 1 LOOP
      FOR p IN 0 .. per - 1 LOOP
        n := n + 1;
        INSERT INTO layout_seat (layout_id, seat_idx, seat_id, coach_code, seat_no, seat_type, x, y)
        VALUES (p_layout, base + n - 1, p_coach || '-' || n, p_coach, n,
                CASE WHEN per = 5 THEN (ARRAY['WINDOW','AISLE','AISLE','MIDDLE','WINDOW'])[p + 1]
                     ELSE (ARRAY['WINDOW','AISLE','AISLE','WINDOW'])[p + 1] END,
                r, CASE WHEN p < 2 THEN p ELSE p + 1 END);
      END LOOP;
    END LOOP;
  ELSIF p_kind IN ('AC_B', 'F_BERTH') THEN
    INSERT INTO layout_coach (layout_id, coach_code, position, class_code, width, height)
    VALUES (p_layout, p_coach, p_pos, cls, 17, 2);
    FOR r IN 0 .. 5 LOOP
      FOR p IN 0 .. 3 LOOP
        n := n + 1;
        INSERT INTO layout_seat (layout_id, seat_idx, seat_id, coach_code, seat_no, seat_type, x, y)
        VALUES (p_layout, base + n - 1, p_coach || '-' || n, p_coach, n,
                CASE WHEN p % 2 = 0 THEN 'BERTH_LOWER' ELSE 'BERTH_UPPER' END, r * 3 + p / 2, p % 2);
      END LOOP;
    END LOOP;
  ELSIF p_kind = 'AC_S' THEN
    INSERT INTO layout_coach (layout_id, coach_code, position, class_code, width, height)
    VALUES (p_layout, p_coach, p_pos, cls, 17, 3);
    FOR r IN 0 .. 5 LOOP
      FOR p IN 0 .. 5 LOOP
        n := n + 1;
        INSERT INTO layout_seat (layout_id, seat_idx, seat_id, coach_code, seat_no, seat_type, x, y)
        VALUES (p_layout, base + n - 1, p_coach || '-' || n, p_coach, n,
                (ARRAY['WINDOW','MIDDLE','AISLE'])[p % 3 + 1], r * 3 + p / 3, p % 3);
      END LOOP;
    END LOOP;
  ELSE
    RAISE EXCEPTION 'unknown coach kind %', p_kind;
  END IF;
END $$;

INSERT INTO layout (layout_id, name, n_seats) OVERRIDING SYSTEM VALUE VALUES
  (10, 'Metre gauge day intercity',   1),
  (11, 'Metre gauge night intercity', 1),
  (12, 'Broad gauge intercity',       1)
ON CONFLICT DO NOTHING;
SELECT setval(pg_get_serial_sequence('layout', 'layout_id'), GREATEST(100, (SELECT max(layout_id) FROM layout)));

SELECT pg_temp.add_coach(l, ord, c, k) FROM (VALUES
  (10, 1, 'KA',   'SNIGDHA'),    (10, 2, 'KHA',  'SNIGDHA'),    (10, 3, 'GA',   'SNIGDHA'),
  (10, 4, 'GHA',  'AC_S'),       (10, 5, 'UMA',  'S_CHAIR_MG'), (10, 6, 'CHA',  'S_CHAIR_MG'),
  (10, 7, 'SCHA', 'S_CHAIR_MG'), (10, 8, 'JA',   'S_CHAIR_MG'), (10, 9, 'JHA',  'S_CHAIR_MG'),
  (10,10, 'NEO',  'S_CHAIR_MG'),

  (11, 1, 'KA',   'AC_B'),       (11, 2, 'KHA',  'AC_B'),       (11, 3, 'GA',   'F_BERTH'),
  (11, 4, 'GHA',  'SNIGDHA'),    (11, 5, 'UMA',  'SNIGDHA'),    (11, 6, 'CHA',  'S_CHAIR_MG'),
  (11, 7, 'SCHA', 'S_CHAIR_MG'), (11, 8, 'JA',   'S_CHAIR_MG'), (11, 9, 'JHA',  'S_CHAIR_MG'),

  (12, 1, 'KA',   'AC_B'),       (12, 2, 'KHA',  'AC_S'),       (12, 3, 'GA',   'SNIGDHA'),
  (12, 4, 'GHA',  'SNIGDHA'),    (12, 5, 'UMA',  'S_CHAIR_BG'), (12, 6, 'CHA',  'S_CHAIR_BG'),
  (12, 7, 'SCHA', 'S_CHAIR_BG'), (12, 8, 'JA',   'S_CHAIR_BG'), (12, 9, 'JHA',  'S_CHAIR_BG')
) v(l, ord, c, k) ORDER BY l, ord;

UPDATE layout l SET n_seats = (SELECT count(*) FROM layout_seat s WHERE s.layout_id = l.layout_id)
 WHERE layout_id IN (10, 11, 12);

INSERT INTO quota_plan (quota_plan_id, name) OVERRIDING SYSTEM VALUE VALUES
  (10, 'Online, metre gauge day'), (11, 'Online, metre gauge night'), (12, 'Online, broad gauge')
ON CONFLICT DO NOTHING;
SELECT setval(pg_get_serial_sequence('quota_plan', 'quota_plan_id'), GREATEST(100, (SELECT max(quota_plan_id) FROM quota_plan)));
INSERT INTO quota_plan_seat (quota_plan_id, seat_idx, quota)
SELECT layout_id, seat_idx, 'ONLINE' FROM layout_seat WHERE layout_id IN (10, 11, 12)
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------
-- Trains. zone: E = eastern (sales open 14:00), W = western (08:00).
-- off: ISO weekday the train does not run (1 = Monday … 7 = Sunday).
-- ---------------------------------------------------------------------------
CREATE TEMP TABLE svc (train_no text, name text, name_bn text, layout int, dep time, off smallint[], zone char) ON COMMIT DROP;
INSERT INTO svc VALUES
  ('787', 'Sonar Bangla Express', 'সোনার বাংলা এক্সপ্রেস', 10, '07:00', '{3}', 'E'),
  ('788', 'Sonar Bangla Express', 'সোনার বাংলা এক্সপ্রেস', 10, '17:00', '{2}', 'E'),
  ('701', 'Subarna Express',      'সুবর্ণ এক্সপ্রেস',      10, '07:00', '{1}', 'E'),
  ('702', 'Subarna Express',      'সুবর্ণ এক্সপ্রেস',      10, '16:30', '{1}', 'E'),
  ('704', 'Mohanagar Provati',    'মহানগর প্রভাতী',      10, '07:45', '{}',  'E'),
  ('703', 'Mohanagar Godhuli',    'মহানগর গোধূলি',       10, '15:00', '{}',  'E'),
  ('742', 'Turna',                'তূর্ণা',               11, '23:15', '{}',  'E'),
  ('741', 'Turna',                'তূর্ণা',               11, '23:30', '{}',  'E'),
  ('814', 'Cox''s Bazar Express', 'কক্সবাজার এক্সপ্রেস',  11, '22:30', '{1}', 'E'),
  ('813', 'Cox''s Bazar Express', 'কক্সবাজার এক্সপ্রেস',  11, '12:30', '{2}', 'E'),
  ('816', 'Parjotak Express',     'পর্যটক এক্সপ্রেস',     10, '06:15', '{7}', 'E'),
  ('815', 'Parjotak Express',     'পর্যটক এক্সপ্রেস',     10, '20:00', '{7}', 'E'),
  ('709', 'Parabat Express',      'পারাবত এক্সপ্রেস',     10, '06:20', '{1}', 'E'),
  ('710', 'Parabat Express',      'পারাবত এক্সপ্রেস',     10, '15:45', '{1}', 'E'),
  ('739', 'Upaban Express',       'উপবন এক্সপ্রেস',       11, '21:50', '{3}', 'E'),
  ('740', 'Upaban Express',       'উপবন এক্সপ্রেস',       11, '23:30', '{3}', 'E'),
  ('753', 'Silk City Express',    'সিল্ক সিটি এক্সপ্রেস',  12, '14:45', '{7}', 'W'),
  ('754', 'Silk City Express',    'সিল্ক সিটি এক্সপ্রেস',  12, '07:15', '{7}', 'W'),
  ('759', 'Padma Express',        'পদ্মা এক্সপ্রেস',       12, '22:45', '{2}', 'W'),
  ('760', 'Padma Express',        'পদ্মা এক্সপ্রেস',       12, '16:00', '{2}', 'W'),
  ('726', 'Sundarban Express',    'সুন্দরবন এক্সপ্রেস',    12, '08:15', '{3}', 'W'),
  ('725', 'Sundarban Express',    'সুন্দরবন এক্সপ্রেস',    12, '21:45', '{2}', 'W');

INSERT INTO train (train_no, name, name_bn, layout_id, off_days)
SELECT train_no, name, name_bn, layout, off FROM svc ON CONFLICT DO NOTHING;

-- Stops: minutes after departure from the origin, and km from the origin.
INSERT INTO train_stop (train_no, idx, station_code, arr_offset_min, dep_offset_min, km) VALUES
  -- Dhaka – Chattogram (321 km)
  ('787',0,'DHK',NULL,0,0),   ('787',1,'BAN',25,27,18),   ('787',2,'CTG',295,NULL,321),
  ('788',0,'CTG',NULL,0,0),   ('788',1,'BAN',268,270,303),('788',2,'DHK',295,NULL,321),
  ('702',0,'DHK',NULL,0,0),   ('702',1,'BAN',25,27,18),   ('702',2,'CTG',295,NULL,321),
  ('701',0,'CTG',NULL,0,0),   ('701',1,'BAN',268,270,303),('701',2,'DHK',295,NULL,321),
  ('704',0,'DHK',NULL,0,0),   ('704',1,'BAN',25,28,18),   ('704',2,'BBR',110,113,81),  ('704',3,'BBA',145,148,107),
  ('704',4,'AKH',165,170,124),('704',5,'CML',225,228,189),('704',6,'FEN',280,283,245), ('704',7,'CTG',350,NULL,321),
  ('703',0,'CTG',NULL,0,0),   ('703',1,'FEN',67,70,76),   ('703',2,'CML',122,125,132), ('703',3,'AKH',180,185,197),
  ('703',4,'BBA',200,203,214),('703',5,'BBR',235,238,240),('703',6,'BAN',325,328,303), ('703',7,'DHK',355,NULL,321),
  ('742',0,'DHK',NULL,0,0),   ('742',1,'BAN',25,28,18),   ('742',2,'BBR',115,118,81),  ('742',3,'AKH',170,175,124),
  ('742',4,'CML',235,238,189),('742',5,'FEN',290,293,245),('742',6,'CTG',360,NULL,321),
  ('741',0,'CTG',NULL,0,0),   ('741',1,'FEN',70,73,76),   ('741',2,'CML',125,128,132), ('741',3,'AKH',185,190,197),
  ('741',4,'BBR',240,243,240),('741',5,'BAN',315,318,303),('741',6,'DHK',345,NULL,321),
  -- Dhaka – Cox's Bazar (471 km, via Chattogram)
  ('814',0,'DHK',NULL,0,0),   ('814',1,'BAN',25,28,18),   ('814',2,'CTG',330,345,321), ('814',3,'COX',530,NULL,471),
  ('813',0,'COX',NULL,0,0),   ('813',1,'CTG',190,200,150),('813',2,'BAN',490,493,453), ('813',3,'DHK',520,NULL,471),
  ('816',0,'DHK',NULL,0,0),   ('816',1,'BAN',25,28,18),   ('816',2,'CTG',325,340,321), ('816',3,'COX',525,NULL,471),
  ('815',0,'COX',NULL,0,0),   ('815',1,'CTG',170,185,150),('815',2,'BAN',480,483,453), ('815',3,'DHK',510,NULL,471),
  -- Dhaka – Sylhet (319 km)
  ('709',0,'DHK',NULL,0,0),   ('709',1,'BAN',25,28,18),   ('709',2,'BBR',105,108,81),  ('709',3,'BBA',140,143,107),
  ('709',4,'SHG',230,233,190),('709',5,'SRM',290,293,222),('709',6,'KUL',335,338,260), ('709',7,'SYL',400,NULL,319),
  ('710',0,'SYL',NULL,0,0),   ('710',1,'KUL',62,65,59),   ('710',2,'SRM',105,108,97),  ('710',3,'SHG',160,163,129),
  ('710',4,'BBA',250,253,212),('710',5,'BBR',285,288,238),('710',6,'BAN',385,388,301), ('710',7,'DHK',415,NULL,319),
  ('739',0,'DHK',NULL,0,0),   ('739',1,'BAN',25,30,18),   ('739',2,'BBR',115,118,81),  ('739',3,'SHG',260,263,190),
  ('739',4,'SRM',320,323,222),('739',5,'KUL',375,378,260),('739',6,'SYL',450,NULL,319),
  ('740',0,'SYL',NULL,0,0),   ('740',1,'KUL',65,68,59),   ('740',2,'SRM',115,118,97),  ('740',3,'SHG',175,178,129),
  ('740',4,'BBR',315,318,238),('740',5,'BAN',400,403,301),('740',6,'DHK',430,NULL,319),
  -- Dhaka – Rajshahi (343 km, via Jamuna bridge)
  ('753',0,'DHK',NULL,0,0),   ('753',1,'BAN',25,28,18),   ('753',2,'JOY',50,53,32),    ('753',3,'TNG',120,123,98),
  ('753',4,'ISD',270,275,241),('753',5,'ABD',300,303,263),('753',6,'RAJ',355,NULL,343),
  ('754',0,'RAJ',NULL,0,0),   ('754',1,'ABD',45,48,80),   ('754',2,'ISD',75,80,102),   ('754',3,'TNG',230,233,245),
  ('754',4,'JOY',300,303,311),('754',5,'BAN',325,328,325),('754',6,'DHK',355,NULL,343),
  ('759',0,'DHK',NULL,0,0),   ('759',1,'BAN',25,28,18),   ('759',2,'JOY',50,53,32),    ('759',3,'TNG',115,118,98),
  ('759',4,'ISD',265,270,241),('759',5,'ABD',295,298,263),('759',6,'RAJ',345,NULL,343),
  ('760',0,'RAJ',NULL,0,0),   ('760',1,'ABD',45,48,80),   ('760',2,'ISD',75,80,102),   ('760',3,'TNG',225,228,245),
  ('760',4,'JOY',290,293,311),('760',5,'BAN',315,318,325),('760',6,'DHK',340,NULL,343),
  -- Dhaka – Khulna (383 km, via Padma Bridge)
  ('726',0,'DHK',NULL,0,0),   ('726',1,'BHG',90,93,85),   ('726',2,'RJB',150,155,140), ('726',3,'PRD',215,218,197),
  ('726',4,'CHD',265,268,240),('726',5,'JSR',340,345,330),('726',6,'KHL',435,NULL,383),
  ('725',0,'KHL',NULL,0,0),   ('725',1,'JSR',95,100,53),  ('725',2,'CHD',170,173,143), ('725',3,'PRD',215,218,186),
  ('725',4,'RJB',280,285,243),('725',5,'BHG',345,348,298),('725',6,'DHK',450,NULL,383)
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------
-- Fare chart: one row per station pair and class served by some train,
-- base + per-km rate rounded to whole taka (poisha), valid from 2024-01-01.
-- ---------------------------------------------------------------------------
CREATE TEMP TABLE rate (class_code text, base_minor int, per_km_minor int) ON COMMIT DROP;
INSERT INTO rate VALUES
  ('S_CHAIR',  60, 140), ('F_SEAT', 127, 213), ('SNIGDHA', 114, 266),
  ('F_BERTH', 110, 290), ('AC_S',   101, 319), ('AC_B',    241, 479);

INSERT INTO fare (class_code, from_station, to_station, valid_from, amount_minor)
SELECT DISTINCT ON (r.class_code, a.station_code, b.station_code)
       r.class_code, a.station_code, b.station_code, DATE '2024-01-01',
       ((r.base_minor + r.per_km_minor * (b.km - a.km) + 50) / 100) * 100
FROM train_stop a
JOIN train_stop b ON b.train_no = a.train_no AND b.idx > a.idx
JOIN train t ON t.train_no = a.train_no
JOIN (SELECT DISTINCT layout_id, class_code FROM layout_coach) c ON c.layout_id = t.layout_id
JOIN rate r ON r.class_code = c.class_code
ORDER BY r.class_code, a.station_code, b.station_code, b.km - a.km
ON CONFLICT DO NOTHING;

INSERT INTO charge_rule (charge_code, per_ticket_minor, valid_from) VALUES ('SERVICE', 2000, DATE '2024-01-01')
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------
-- Journeys for today and the next 11 days, skipping each train's off day.
-- journey_id = train number * 10^8 + YYYYMMDD, e.g. 78720261102.
-- ---------------------------------------------------------------------------
CREATE TEMP TABLE run ON COMMIT DROP AS
SELECT s.*, d::date AS run_date,
       s.train_no::bigint * 100000000 + to_char(d, 'YYYYMMDD')::bigint AS journey_id,
       (d::date + s.dep) AT TIME ZONE 'Asia/Dhaka' AS departs_at,
       ((d::date - 10) + CASE s.zone WHEN 'E' THEN time '14:00' ELSE time '08:00' END) AT TIME ZONE 'Asia/Dhaka' AS opens_at
FROM svc s, generate_series(current_date, current_date + 11, interval '1 day') d
WHERE NOT (extract(isodow FROM d)::smallint = ANY (s.off));

INSERT INTO journey (journey_id, train_no, run_date, departs_at, n_segments, layout_id, quota_plan_id, sales_close_at)
SELECT r.journey_id, r.train_no, r.run_date, r.departs_at,
       (SELECT max(idx) FROM train_stop t WHERE t.train_no = r.train_no), r.layout, r.layout,
       r.departs_at - interval '30 minutes'
FROM run r
ON CONFLICT DO NOTHING;

INSERT INTO journey_pool (journey_id, class_code, quota, opens_at, max_seats_per_user)
SELECT r.journey_id, c.class_code, 'ONLINE', r.opens_at, 4
FROM run r JOIN (SELECT DISTINCT layout_id, class_code FROM layout_coach) c ON c.layout_id = r.layout
ON CONFLICT DO NOTHING;

INSERT INTO scheduler_job (journey_id, kind, due_at)
SELECT journey_id, 'WARM', opens_at - interval '10 minutes' FROM run
UNION ALL
SELECT journey_id, 'OPEN', opens_at FROM run
UNION ALL
SELECT journey_id, 'CLOSE', departs_at - interval '30 minutes' FROM run
ON CONFLICT DO NOTHING;

-- Monthly booking / ticket partitions for the whole sales horizon.
SELECT create_month_partitions(m::date)
FROM generate_series(date_trunc('month', current_date), date_trunc('month', current_date + 41), interval '1 month') m;

COMMIT;
