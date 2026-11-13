# Writing the documents

For a contributor. The rules, short:

- **The README answers, in this order**: what it is and what problem it solves (one sentence); when to use it and when not; a picture of the parts; a try-it that works in a minute with nothing to prepare, showing what each
  command prints; the ways to install; a short glossary; the other documents grouped by what the reader wants to do. Internals and status go last.
- **One kind of page** ([Diataxis](https://diataxis.fr/)): a tutorial (one path that works, the output after each step), a how-to (one task), a reference (complete, dry, shaped like the program), an explanation (why).
  Do not mix them; link instead. [docs/README.md](README.md) says which kind each page is.
- **A command always comes with what it prints.** Say when the output is cut. A tutorial that cannot be followed to the end is a bug.
- **State defaults and requirements** in a reference: what is required, what happens when a setting is left out ("no limit" is a default and is written), whether a change needs a restart. Dangerous settings are marked.
- **Say what was and was not run**, on what and when (absolute dates, never "recently"). A claim that was never run is written "not run". Never leave a claim in a page that the code does not do.
- **Keep the pages true with tests where the program can be asked**: `cmd/blobgate/docs_test.go` fails when a setting, a metric, a command, a flag or an error code has no line in its page; the description of the API
  is checked against the routes. A new setting is its line in the same change.
- English, plain, short sentences, the program's own words; define a term or link it.
