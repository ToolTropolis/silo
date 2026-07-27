-- Which repository a project belongs to.
--
-- Purely informational: nothing in the read, write, or isolation path consults
-- these columns. They exist because a project ID is a storage identifier
-- ("myservice") and six months later nobody remembers which repository that
-- was — especially once the name has been normalized away from the repo's own
-- ("My_Service.v2" became "my-service-v2").
--
-- Nullable because a project can legitimately have no repo: one created by
-- siloctl, one shared by several repos, or one onboarded before this existed.
-- An empty string and NULL both mean "not recorded"; neither is an error.
ALTER TABLE projects ADD COLUMN repo_url TEXT;
ALTER TABLE projects ADD COLUMN repo_path TEXT;
