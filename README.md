# timegit

Easily keep your time records when working in a Git repo, using Kimai as the backing service.

There are plans to support other backends, such as SQL databases, spreadsheets (including Google Sheets), or structured text files (such as JSON files).

## Installation

Assuming you have Go installed:

`go install github.com/y3ro/timegit@latest`

You need to have `$HOME/go/bin` in your `PATH`.

## Naming conventions

The local folder containing the Git repository should have the same name as the corresponding project in Kimai.

The name of the current Git branch can also correspond to that of an activity for the Kimai project to start it.
If it doesn't, a default activity for the project is selected (see `-start` option below).

## Usage

First you will need to create the configuration file `$HOME/.config/timegit.json` (or specify your own filepath with the `-config` option).
Example contents:

```
{
    "KimaiUrl": "https://timetracking.domain.com",
    "KimaiUsername": "username",
    "KimaiPassword": "password",
    "HourlyRate": 100,
    "ProjectMap": {
        "project1": 0,
        "project2": 1
    }
}
```

You can get a project map of your Kimai instance using a specific option (`list-projs`), as shown below. You would only need to copy and paste the result.

Then, just run from you project's folder:

```
timegit <option>
```

Avaliable options:

* `-start`: Starts a record for the activity in your Kimai instance correspoding to the current project and Git branch.
If there is no match for the current branch, it will start the default activity for the project. 
If this default activity, which has the same name of the project, does not exists, it will try to create it. 
* `-stop`: Stops all current active records in your Kimai instance.
* `-restart`: Starts a new record for the last activity stopped.
* `-list-projs`: Prints the map of the registered projects in your Kimai instance, which you can copy to use in your configuration file. 
* `-config <filepath>`: Specifies the path to the configuration file. If not specified, the default configuration file is in `$HOME/.config/timegit.json`. 

### Integration with Git

You can call this program from the `post-checkout` hook in your Git repository to further automate the process.
Just add the following line:

```
timegit -stop -start
```
