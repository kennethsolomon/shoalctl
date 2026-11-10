Choosing [tests].command and format

init fills these in only for Go modules. For any other stack, set them once
per project. command is what you would type to run the full suite. format
is how shoal reads the result. Pick the first answer that applies:

1. The project has no tests: none. There is no regression gate.
2. It is a Go module: go-json.
3. The test runner can write a JUnit XML report (look for a flag like
   --log-junit, --junitxml or --reporter=junit): junit, with {junit} where
   the report path goes.
4. Otherwise: exit-code. Any command works, but shoal only sees pass or
   fail for the whole suite, so the merge gate cannot name which test broke.

Prefer junit (or go-json) whenever the runner supports it, because the
zero-regression gate compares results test by test.
bugfix and hotfix need format junit or go-json.

format     stack: command
junit      Laravel (PHPUnit): php artisan test --log-junit {junit}
junit      Laravel (Pest): vendor/bin/pest --log-junit {junit}
junit      PHPUnit: vendor/bin/phpunit --log-junit {junit}
go-json    Go: go test -json ./...   (written by init)
junit      Vitest: npx vitest run --reporter=junit --outputFile={junit}
junit      Jest (needs jest-junit):
             JEST_JUNIT_OUTPUT_FILE={junit} npx jest --reporters=jest-junit
junit      pytest: pytest --junitxml={junit}
exit-code  Script with no test runner: a smoke check, for example
             python3 -m py_compile app.py
none       No tests: ""

In the config it looks like this:
   [tests]
   command = "php artisan test --log-junit {junit}"
   format = "junit"

If the suite needs git-ignored files (.env, vendor/, node_modules/, a
virtualenv), also set [tests].setup, for example
   setup = "cp {root}/.env . && composer install"
The baseline runs in a clean worktree that does not have them.
