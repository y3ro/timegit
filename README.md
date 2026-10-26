# timegit

Easily keep your time records when working in a Git repo, using Kimai as the backing service.

There are plans to support other backends, such as SQL databases, spreadsheets (including Google Sheets), or structured text files (such as JSON files).

## Installation

Assuming you have Go installed:

`go install github.com/y3ro/timegit@latest`

You need to have `$HOME/go/bin` in your `PATH`.

## Naming conventions

The project is the name of the local folder containing the Git repository, which should match a project in your `ProjectMap`.
If the folder has another name (for example, in a worktree or a clone with a custom name), set the project explicitly:

```
git config timegit.project project1
```

The activity for the current branch is chosen in this order:

1. The activity of the first entry in `ActivityRules` whose `Branch` pattern matches the branch.
2. The activity with the same name as the branch.
3. The default activity of the project, which has the same name as the project (see `-start` below).
It is also used when HEAD is detached.

Records are created with the branch name as their description, so you can tell them apart even when they share an activity.

## Usage

First you will need to create the configuration file `$HOME/.config/timegit.json` (or specify your own filepath with the `-config` option).
Example contents:

```
{
    "KimaiUrl": "https://timetracking.domain.com",
    "KimaiApiToken": "api-token",
    "HourlyRate": 100,
    "MaxRecordHours": 8,
    "IdleMinutes": 30,
    "ProjectMap": {
        "project1": 1,
        "project2": 2
    },
    "ActivityRules": [
        {"Branch": "feature/*", "Activity": "Development"},
        {"Branch": "fix/*", "Activity": "Bugfixing"}
    ]
}
```

* `KimaiUrl`: base URL of your Kimai instance, without the `/api` suffix.
* `KimaiApiToken`: a Kimai API token, sent as `Authorization: Bearer <token>`.
On older Kimai instances you can use `KimaiUsername` and `KimaiPassword` (the API password) instead.
* `HourlyRate` (optional): rate applied to new records. If omitted, Kimai applies the rates configured for the project or activity.
* `MaxRecordHours` (optional, 8 by default): records longer than this are reported with a warning, since they were probably not stopped on time.
* `IdleMinutes` (optional, 30 by default): how long a repository must go without activity for `-stop-idle` to stop its records.
* `ProjectMap`: Kimai project ids by project name.
* `ActivityRules` (optional): maps branch patterns to Kimai activities. In patterns, `*` matches any sequence of characters (including `/`) and `?` any single character.
If the activity of a matching rule does not exist in Kimai, a warning is shown and the next options are tried.

You can get a project map of your Kimai instance using a specific option (`list-projs`), as shown below. You would only need to copy and paste the result.

Then, just run from you project's folder:

```
timegit <option>
```

Available options:

* `-start`: Starts a record for the activity in your Kimai instance corresponding to the current project and Git branch (see [Naming conventions](#naming-conventions)).
If a record for the same project, activity and branch is already active, it does nothing.
If the default activity of the project does not exist, it will try to create it.
It also remembers the repository of the project in `$HOME/.config/timegit-state.json`, for `-stop-idle`.
* `-stop`: Stops the active records of the current project. Outside a Git repository, it stops all of them.
* `-all`: Used with `-stop`, stops the active records of every project.
* `-restart`: Starts a new record for the last activity stopped.
* `-stop-idle`: Stops the active records whose repository has had no activity for `IdleMinutes` (see [Stopping idle records](#stopping-idle-records)).
* `-status`: Shows the active records and how long they have been running.
* `-list-projs`: Prints the map of the registered projects in your Kimai instance, which you can copy to use in your configuration file.
* `-install-hook`: Installs the Git hook described below in the current repository.
* `-config <filepath>`: Specifies the path to the configuration file. If not specified, the default configuration file is in `$HOME/.config/timegit.json`.

When `-stop` and `-start` are used together, the record of the current branch is kept running if it is already active, and the rest are stopped.

### Integration with Git

Run this from your repository to switch the Kimai record automatically whenever you switch branches:

```
timegit -install-hook
```

It installs a `post-checkout` hook (respecting `core.hooksPath`) that runs `timegit -stop -all -start` only on branch checkouts.
File checkouts (`git checkout -- file`) and checkouts that leave HEAD detached, such as rebases, bisects or tag checkouts, are ignored.
If you already have a `post-checkout` hook, it is left untouched and the lines to add to it are printed instead.

### Stopping idle records

`timegit -stop-idle` stops the active records whose repository has had no activity for `IdleMinutes`, and sets their end time to the last activity, so idle time is not tracked.
The activity of a repository is the latest of:

* The last time HEAD moved: commits, checkouts, merges, rebases, resets...
* The last modification of a changed or untracked file. Ignored files, such as build outputs, do not count.
* The start of the record, so a record you have just started or restarted is not stopped right away.

It checks the repository each project was last started from with `-start`, plus the current one, so it can run from any folder.
Records of projects with no known repository are skipped.

It is meant to run periodically. For example, every 10 minutes with cron:

```
*/10 * * * * $HOME/go/bin/timegit -stop-idle
```

Or with the Windows Task Scheduler:

```
schtasks /create /sc minute /mo 10 /tn timegit-stop-idle /tr "%USERPROFILE%\go\bin\timegit.exe -stop-idle"
```

Activity outside the repository, such as reading code or attending a meeting, is not detected, so choose an `IdleMinutes` value that fits how you work.
