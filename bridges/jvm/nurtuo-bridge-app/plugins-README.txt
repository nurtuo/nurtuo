Nurtuo JVM bridge: plugins directory

Put customer-supplied jars here (they are never bundled with the bridge), for example:

  IBM MQ     com.ibm.mq.jakarta.client-<version>.jar (+ its dependencies, see IBM's docs)
  SAP JCo    sapjco3.jar (and the native sapjco3 library on -Djava.library.path)
  JDBC       vendor driver jars

Every *.jar in this directory (and one level of subdirectories) is added to the connector
classpath at startup. Restart the bridge after changing the contents.
