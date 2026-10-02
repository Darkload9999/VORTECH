-- Read side for company, departments and employees. Every query is scoped by
-- company_id so one company's data never leaks into another world.

-- name: GetActiveCompany :one
SELECT c.id, c.code, c.name, c.legal_name, c.industry, c.description, c.headquarters,
       c.founded_year, c.email_domain,
       s.slug AS scenario_slug, s.name AS scenario_name, s.version AS scenario_version
FROM companies c
JOIN scenarios s ON s.id = c.scenario_id
WHERE s.is_active;

-- name: GetCompanyStats :one
SELECT
    (SELECT count(*) FROM departments d WHERE d.company_id = @company_id)::int AS departments,
    (SELECT count(*) FROM employees e WHERE e.company_id = @company_id)::int  AS employees,
    (SELECT count(*) FROM assets a WHERE a.company_id = @company_id)::int     AS assets,
    (SELECT count(*) FROM world_zones z WHERE z.company_id = @company_id)::int AS zones;

-- name: ListDepartments :many
SELECT d.id, d.code, d.name, d.description,
       p.code         AS parent_code,
       h.id           AS head_id,
       h.display_name AS head_name,
       h.job_title    AS head_title,
       (SELECT count(*) FROM employees e WHERE e.department_id = d.id)::int AS employee_count
FROM departments d
LEFT JOIN departments p ON p.id = d.parent_id
LEFT JOIN employees h ON h.id = d.head_employee_id
WHERE d.company_id = @company_id
ORDER BY d.sort_order, d.code;

-- name: ListEmployees :many
-- Keyset pagination on employee_code. search is an ILIKE pattern built and
-- escaped by the caller.
SELECT e.id, e.employee_code, e.display_name, e.job_title, e.status, e.employment_type, e.is_npc,
       d.code AS department_code, d.name AS department_name,
       l.code AS location_code
FROM employees e
JOIN departments d ON d.id = e.department_id
LEFT JOIN locations l ON l.id = e.home_location_id
WHERE e.company_id = @company_id
  AND (sqlc.narg(department)::text IS NULL OR d.code = sqlc.narg(department)::text)
  AND (sqlc.narg(search)::text IS NULL
       OR e.display_name ILIKE sqlc.narg(search)::text
       OR e.job_title ILIKE sqlc.narg(search)::text
       OR e.employee_code ILIKE sqlc.narg(search)::text)
  AND (sqlc.narg(after)::text IS NULL OR e.employee_code > sqlc.narg(after)::text)
ORDER BY e.employee_code
LIMIT @page_size;

-- name: GetEmployee :one
SELECT e.id, e.employee_code, e.first_name, e.last_name, e.display_name, e.job_title,
       e.status, e.employment_type, e.is_npc, e.greeting,
       d.id AS department_id, d.code AS department_code, d.name AS department_name,
       m.id AS manager_id, m.employee_code AS manager_code, m.display_name AS manager_name,
       l.id AS location_id, l.code AS location_code, l.name AS location_name,
       z.id AS zone_id, z.code AS zone_code
FROM employees e
JOIN departments d ON d.id = e.department_id
LEFT JOIN employees m ON m.id = e.manager_id
LEFT JOIN locations l ON l.id = e.home_location_id
LEFT JOIN world_zones z ON z.id = l.zone_id
WHERE e.company_id = @company_id AND e.id = @id;

-- name: ListDirectReports :many
SELECT id, employee_code, display_name, job_title
FROM employees
WHERE company_id = @company_id AND manager_id = @manager_id
ORDER BY employee_code;

-- name: ListEmployeeIdentities :many
SELECT i.id, i.username, i.email, i.identity_type, i.status, i.mfa_enabled, i.privileged, i.expires_at,
       COALESCE(
           (SELECT array_agg(g.asset_code ORDER BY g.asset_code)
            FROM asset_relationships r JOIN assets g ON g.id = r.target_asset_id
            WHERE r.source_identity_id = i.id AND r.relationship_type = 'MEMBER_OF'),
           '{}')::text[] AS groups
FROM enterprise_identities i
WHERE i.company_id = @company_id AND i.employee_id = @employee_id
ORDER BY i.username;

-- name: ListEmployeeSchedule :many
SELECT s.weekday, s.starts_at, s.ends_at, s.activity, l.code AS location_code
FROM employee_schedules s
JOIN employees e ON e.id = s.employee_id
LEFT JOIN locations l ON l.id = s.location_id
WHERE e.company_id = @company_id AND s.employee_id = @employee_id
ORDER BY s.weekday, s.starts_at;

-- name: ListEmployeeAssets :many
SELECT id, asset_code, name, asset_type, hostname
FROM assets
WHERE company_id = @company_id AND owner_employee_id = @employee_id
ORDER BY asset_code;
