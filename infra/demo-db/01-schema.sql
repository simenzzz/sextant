-- Postgres seed for the live demo.
--
-- PLACEHOLDER. PLAN.md section 11, open decision #1 (which corpus a recruiter
-- finds legible in ten seconds) is due at P1 and is not decided here. What
-- this file does establish is the shape the real corpus has to arrive in:
-- a read-only role, a schema with a genuine bridge table, and at least one
-- encoded column whose values matter more than its name.
--
-- Mirrors infra/fixtures/toy.sql so the dialect adapters (P3) have the same
-- logical schema to target in both SQLite and Postgres.

CREATE TABLE customers (
    id      INTEGER PRIMARY KEY,
    name    TEXT NOT NULL,
    country TEXT NOT NULL,
    joined  DATE NOT NULL
);

CREATE TABLE products (
    id       INTEGER PRIMARY KEY,
    name     TEXT NOT NULL,
    category TEXT NOT NULL,
    price    NUMERIC(10, 2) NOT NULL
);

CREATE TABLE orders (
    id          INTEGER PRIMARY KEY,
    customer_id INTEGER NOT NULL REFERENCES customers(id),
    placed_at   DATE NOT NULL,
    -- 'P' pending, 'S' shipped, 'C' cancelled. Encoded on purpose: a question
    -- about cancelled orders is unanswerable from column names alone, so the
    -- retriever has to sample distinct values.
    status      CHAR(1) NOT NULL CHECK (status IN ('P', 'S', 'C'))
);

CREATE TABLE order_items (
    order_id   INTEGER NOT NULL REFERENCES orders(id),
    product_id INTEGER NOT NULL REFERENCES products(id),
    quantity   INTEGER NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (order_id, product_id)
);

INSERT INTO customers (id, name, country, joined) VALUES
    (1, 'Amal Haddad', 'LB', '2025-01-14'),
    (2, 'Rita Khoury', 'LB', '2025-03-02'),
    (3, 'Omar Nassif', 'JO', '2025-06-20'),
    (4, 'Lena Farah',  'FR', '2026-02-11');

INSERT INTO products (id, name, category, price) VALUES
    (1, 'Sextant',        'instruments', 240.00),
    (2, 'Marine Compass', 'instruments',  85.50),
    (3, 'Chart Dividers', 'accessories',  18.00),
    (4, 'Logbook',        'accessories',  12.75);

INSERT INTO orders (id, customer_id, placed_at, status) VALUES
    (1, 1, '2026-01-05', 'S'),
    (2, 1, '2026-02-18', 'C'),
    (3, 2, '2026-03-09', 'S'),
    (4, 3, '2026-04-22', 'P'),
    (5, 4, '2026-05-30', 'C');

INSERT INTO order_items (order_id, product_id, quantity) VALUES
    (1, 1, 1), (1, 3, 2), (2, 2, 1), (3, 4, 3), (3, 1, 1), (4, 2, 2), (5, 3, 1);
