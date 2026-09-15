package log

import stdlog "log"

func Debugln(format string, v ...any) { stdlog.Printf(format, v...) }
func Infoln(format string, v ...any)  { stdlog.Printf(format, v...) }
func Warnln(format string, v ...any)  { stdlog.Printf(format, v...) }
func Errorln(format string, v ...any) { stdlog.Printf(format, v...) }
