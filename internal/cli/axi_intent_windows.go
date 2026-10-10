package cli

// Windows has no Unix FIFO open flag. readAxiIntentFile checks the file type
// before opening and again on the opened handle.
const intentFileNonblock = 0
