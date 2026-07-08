@ECHO OFF

IF "%GOPATH%"=="" GOTO NOGO
IF NOT EXIST %GOPATH%\bin\rsrc.exe GOTO INSTALL
:POSTINSTALL
ECHO Creating pkg/deej/cmd/main.syso
%GOPATH%\bin\rsrc -manifest pkg\deej\assets\deej.manifest -ico assets\deej_x.ico -o pkg\deej\cmd\main.syso
GOTO DONE

:INSTALL
ECHO Installing rsrc...
go install github.com/akavel/rsrc@latest
IF ERRORLEVEL 1 GOTO INSTALLFAIL
GOTO POSTINSTALL

:INSTALLFAIL
ECHO Failure running go install github.com/akavel/rsrc@latest.  Ensure that go and git are in PATH
GOTO DONE

:NOGO
ECHO GOPATH environment variable not set
GOTO DONE

:DONE
