package main

import (
	"flag"
	"fmt"
	"github.com/jmoiron/sqlx"
	"github.com/kshvakov/clickhouse"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"
)

var (
	Trace   *log.Logger
	Info    *log.Logger
	Warning *log.Logger
	Error   *log.Logger
)

var (
	ClickhouseConnectionString string
	NoFreezeFlag               bool
	SourceDirectory            string
	DestinationDirectory       string
)

type partitionDescribe struct {
	databaseName string
	tableName    string
	partID       string
}

type dataBase struct {
	name string
}

type GetPartitions struct {
	Database string
	Result   []partitionDescribe
}

type FreezePartitions struct {
	Partitions []partitionDescribe
}

type GetDatabasesList struct {
	Result []dataBase
}

func Init(
	traceHandle io.Writer,
	infoHandle io.Writer,
	warningHandle io.Writer,
	errorHandle io.Writer) {

	Trace = log.New(traceHandle,
		"TRACE: ",
		log.Ldate|log.Ltime|log.Lshortfile)

	Info = log.New(infoHandle,
		"INFO: ",
		log.Ldate|log.Ltime|log.Lshortfile)

	Warning = log.New(warningHandle,
		"WARNING: ",
		log.Ldate|log.Ltime|log.Lshortfile)

	Error = log.New(errorHandle,
		"ERROR: ",
		log.Ldate|log.Ltime|log.Lshortfile)
}

// recursive copy directory and files
func copyDirectory(sourceDirectory string, destinationDirectory string) error {

	var (
		err             error
		fileDescriptors []os.FileInfo
		sourceInfo      os.FileInfo
	)

	if sourceInfo, err = os.Stat(sourceDirectory); err != nil {
		return err
	}

	if err = os.MkdirAll(destinationDirectory, sourceInfo.Mode()); err != nil {
		return err
	}

	if fileDescriptors, err = ioutil.ReadDir(sourceDirectory); err != nil {
		return err
	}
	for _, fileDescriptor := range fileDescriptors {
		sourcePath := path.Join(sourceDirectory, fileDescriptor.Name())
		destinationPath := path.Join(destinationDirectory, fileDescriptor.Name())
		if fileDescriptor.IsDir() {
			if err = copyDirectory(sourcePath, destinationPath); err != nil {
				Error.Fatalln(err)
			}
		} else {
			if err = copyFile(sourcePath, destinationPath); err != nil {
				Error.Fatalln(err)
			}
		}
	}
	return nil
}

// copy files
func copyFile(sourceFile string, destinationFile string) error {

	input, err := ioutil.ReadFile(sourceFile)
	if err != nil {
		Error.Fatalf("cant't open file: %v", sourceFile)
		return err
	}

	err = ioutil.WriteFile(destinationFile, input, 0644)
	if err != nil {
		Error.Fatalf("cant'r create: %v", destinationFile)
		return err
	}

	return nil

}

//Create list of directories
func createDirectories(directoriesList []string) (error, string) {

	for _, currentDirectory := range directoriesList {
		err := os.Mkdir(currentDirectory, os.ModePerm)
		if err != nil {
			return err, currentDirectory
		}
	}
	return nil, ""

}

//Check directory is exist
func isDirectoryExist(directoriesList ...string) (error, string) {

	for _, currentDirectory := range directoriesList {
		if _, err := os.Stat(currentDirectory); os.IsNotExist(err) {
			return err, currentDirectory
		}
	}
	return nil, ""
}

//Get databases list from server
func (gd *GetDatabasesList) Run(databaseConnection *sqlx.DB) error {

	var (
		err       error
		databases []struct {
			DatabaseName string `db:"name"`
		}
	)

	err = databaseConnection.Select(&databases, "show databases;")
	if err != nil {
		return err
	}

	for _, item := range databases {
		gd.Result = append(gd.Result, dataBase{
			name: item.DatabaseName,
		})
	}

	return nil

}

//Freeze partitions and create hardlink in $CLICKHOUSE_DIRECTORY/shadow
func (fz *FreezePartitions) Run(databaseConnection *sqlx.DB) error {

	for _, partition := range fz.Partitions {

		if NoFreezeFlag {
			Info.Printf("ALTER TABLE %v.%v FREEZE PARTITION '%v';",
				partition.databaseName,
				partition.tableName,
				partition.partID,
			)
		} else {

			//freeze partitions
			_, err := databaseConnection.Exec(
				fmt.Sprintf(
					"ALTER TABLE %v.%v FREEZE PARTITION '%v';",
					partition.databaseName,
					partition.tableName,
					partition.partID,
				))
			if err != nil {
				return err
			}

			//copy partition files and metadata
			outDirectory := SourceDirectory
			inDirectory := DestinationDirectory

			directoryList := []string{
				outDirectory + "/partitions",
				outDirectory + "/partitions/" + partition.databaseName,
				outDirectory + "/metadata",
				outDirectory + "/metadata/" + partition.databaseName,
			}

			err, failDirectory := createDirectories(directoryList)
			if err != nil {
				Error.Printf("can't create directory: %v", failDirectory)
				return err
			}

			err = copyDirectory(
				inDirectory+"/shadow/1/data/"+partition.databaseName,
				outDirectory+"/partitions/"+partition.databaseName)
			if err != nil {
				return err
			}

			err = copyDirectory(
				inDirectory+"/metadata/"+partition.databaseName,
				outDirectory+"/metadata/"+partition.databaseName)
			if err != nil {
				return err
			}
		}
	}

	return nil

}

//Get list of partitions for tables
func (gp *GetPartitions) Run(databaseConnection *sqlx.DB) error {

	var (
		err        error
		partitions []struct {
			Partition string `db:"partition"`
			Table     string `db:"table"`
			Database  string `db:"database"`
		}
	)

	err = databaseConnection.Select(&partitions,
		fmt.Sprintf("select "+
			"partition, "+
			"table, "+
			"database "+
			"FROM system.parts WHERE active AND database ='%v';", gp.Database))
	if err != nil {
		return err
	}

	for _, item := range partitions {
		if !strings.HasPrefix(item.Table, ".") {
			Info.Printf("found %v partition of %v table in %v database", item.Partition, item.Table, item.Database)
			gp.Result = append(gp.Result, partitionDescribe{
				partID:       item.Partition,
				tableName:    item.Table,
				databaseName: item.Database,
			})
		}
	}

	return nil

}

func main() {

	var (
		err             error
		inputDirectory  string
		outputDirectory string
	)

	Init(ioutil.Discard, os.Stdout, os.Stdout, os.Stderr)

	argBackup := flag.Bool("backup", false, "backup mode")
	argRestore := flag.Bool("restore", false, "restore mode")
	argHost := flag.String("h", "127.0.0.1", "server hostname")
	argDataBase := flag.String("db", "", "database name")
	argDebugOn := flag.Bool("d", false, "show debug info")
	argPort := flag.String("p", "9000", "server port")
	argNoFreeze := flag.Bool("no-freeze", false, "do not freeze, only show partitions")
	argInDirectory := flag.String("in", "", "source directory (/var/lib/clickhouse for backup mode by default)")
	argOutDirectory := flag.String("out", "", "destination directory")

	flag.Parse()

	NoFreezeFlag = *argNoFreeze
	ClickhouseConnectionString = "tcp://" + *argHost + ":" + *argPort + "?username=&compress=true"

	if *argDebugOn {
		ClickhouseConnectionString = ClickhouseConnectionString + "&debug=true"
	}

	// make connection to clickhouse server
	clickhouseConnection, err := sqlx.Open("clickhouse", ClickhouseConnectionString)
	if err != nil {
		Error.Fatalf("can't connect to clickouse server, v%", err)
	}

	defer clickhouseConnection.Close()

	if err = clickhouseConnection.Ping(); err != nil {
		if exception, ok := err.(*clickhouse.Exception); ok {
			Error.Fatalf("[%d] %s \n%s\n", exception.Code, exception.Message, exception.StackTrace)
		} else {
			Error.Fatalln(err)
		}
	}

	//Determine run mode
	if *argBackup && !*argRestore { //Backup mode

		Info.Println("Run in backup mode")

		if *argInDirectory == "" {
			inputDirectory = "/var/lib/clickhouse"
		} else {
			inputDirectory = *argInDirectory
		}

		if *argOutDirectory == "" {
			Error.Fatalln("please set destination directory")
		} else {
			outputDirectory = *argOutDirectory
		}

		err, noDirectory := isDirectoryExist(inputDirectory, outputDirectory)
		if err != nil {
			Error.Fatalf("v% not found", noDirectory)
		}

		var partitionsList []partitionDescribe

		//Get partitions list for databases or database (--db argument)
		if *argDataBase == "" {
			databaseList := GetDatabasesList{}
			err = databaseList.Run(clickhouseConnection)
			if err != nil {
				Error.Printf("can't get database list, %v", err)
			}
			for _, database := range databaseList.Result {
				cmdGetPartitionsList := GetPartitions{Database: database.name}
				err = cmdGetPartitionsList.Run(clickhouseConnection)
				if err != nil {
					Error.Printf("can't get partition list, %v", err)
				}
				partitionsList = cmdGetPartitionsList.Result
			}
		} else {
			cmdGetPartitionsList := GetPartitions{Database: *argDataBase}
			err = cmdGetPartitionsList.Run(clickhouseConnection)
			if err != nil {
				Error.Printf("can't get partition list, %v", err)
			}
			partitionsList = cmdGetPartitionsList.Result
		}

		cmdFreezePartitions := FreezePartitions{Partitions: partitionsList}
		err = cmdFreezePartitions.Run(clickhouseConnection)
		if err != nil {
			Error.Printf("can't freeze partition, %v", err)
		}

	} else if *argRestore && !*argBackup {
		fmt.Println("Run in restore mode")

	} else if !*argRestore && !*argBackup {
		fmt.Println("Choose mode (restore tor backup)")

	} else {
		Error.Fatalln("Run in only one mode (backup or restore)")

	}

	fmt.Println("done")
}
